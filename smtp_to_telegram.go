package main

import (
	"errors"
	"fmt"
	"github.com/flashmob/go-guerrilla"
	"github.com/flashmob/go-guerrilla/backends"
	"github.com/flashmob/go-guerrilla/log"
	"github.com/flashmob/go-guerrilla/mail"
	"github.com/jhillyerd/enmime"
	"github.com/urfave/cli/v2"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

var (
	Version string = "UNKNOWN_RELEASE"
)

type SmtpConfig struct {
	smtpListen      string
	smtpPrimaryHost string
}

type TelegramConfig struct {
	telegramChatIds   string
	telegramBotToken  string
	telegramApiPrefix string
	vkChatIds   string
	vkBotToken  string
	vkApiPrefix string
	messageTemplate   string
}

func GetHostname() string {
	hostname, err := os.Hostname()
	if err != nil {
		panic(fmt.Sprintf("Unable to detect hostname: %s", err))
	}
	return hostname
}

func main() {
	app := cli.NewApp()
	app.Name = "smtp_to_telegram"
	app.Usage = "A small program which listens for SMTP and sends " +
		"all incoming Email messages to Telegram."
	app.Version = Version
	app.Action = func(c *cli.Context) error {
		// Required flags are not supported, see https://github.com/urfave/cli/issues/85
		if !c.IsSet("telegram-chat-ids") {
			return cli.NewExitError("Telegram chat ids are missing. See `--help`", 2)
		}
		if !c.IsSet("telegram-bot-token") {
			return cli.NewExitError("Telegram bot token is missing. See `--help`", 2)
		}
		smtpConfig := &SmtpConfig{
			smtpListen:      c.String("smtp-listen"),
			smtpPrimaryHost: c.String("smtp-primary-host"),
		}
		telegramConfig := &TelegramConfig{
			telegramChatIds:   c.String("telegram-chat-ids"),
			telegramBotToken:  c.String("telegram-bot-token"),
			telegramApiPrefix: c.String("telegram-api-prefix"),
			vkChatIds: c.String("vk-chat-ids"),
			vkBotToken: c.String("vk-bot-token"),
			vkApiPrefix: c.String("vk-api-prefix"),
			messageTemplate:   c.String("message-template"),
		}
		d, err := SmtpStart(smtpConfig, telegramConfig)
		if err != nil {
			panic(fmt.Sprintf("start error: %s", err))
		}
		sigHandler(d)
		return nil
	}
	app.Flags = []cli.Flag{
		&cli.StringFlag{
			Name:    "smtp-listen",
			Value:   "127.0.0.1:2525",
			Usage:   "SMTP: TCP address to listen to",
			EnvVars: []string{"ST_SMTP_LISTEN"},
		},
		&cli.StringFlag{
			Name:    "smtp-primary-host",
			Value:   GetHostname(),
			Usage:   "SMTP: primary host",
			EnvVars: []string{"ST_SMTP_PRIMARY_HOST"},
		},
		&cli.StringFlag{
			Name:    "telegram-chat-ids",
			Usage:   "Telegram: comma-separated list of chat ids",
			EnvVars: []string{"ST_TELEGRAM_CHAT_IDS"},
		},
		&cli.StringFlag{
			Name:    "telegram-bot-token",
			Usage:   "Telegram: bot token",
			EnvVars: []string{"ST_TELEGRAM_BOT_TOKEN"},
		},
		&cli.StringFlag{
			Name:    "telegram-api-prefix",
			Usage:   "Telegram: API url prefix",
			Value:   "https://api.telegram.org/",
			EnvVars: []string{"ST_TELEGRAM_API_PREFIX"},
		},
		&cli.StringFlag{
			Name:    "vk-chat-ids",
			Usage:   "vk: comma-separated list of chat ids",
			EnvVars: []string{"ST_VK_CHAT_IDS"},
		},
		&cli.StringFlag{
			Name:    "vk-bot-token",
			Usage:   "vk: bot token",
			EnvVars: []string{"ST_VK_BOT_TOKEN"},
		},
		&cli.StringFlag{
			Name:    "vk-api-prefix",
			Usage:   "vk: API url prefix",
			Value:   "https://api.vk.com",
			EnvVars: []string{"ST_VK_API_PREFIX"},
		},		
		&cli.StringFlag{
			Name:    "message-template",
			Usage:   "Telegram message template",
			Value:   "From: {from}\\nTo: {to}\\nSubject: {subject}\\n\\n{body}",
			EnvVars: []string{"ST_TELEGRAM_MESSAGE_TEMPLATE"},
		},
	}
	err := app.Run(os.Args)
	if err != nil {
		os.Exit(1)
	}
}

func SmtpStart(
	smtpConfig *SmtpConfig, telegramConfig *TelegramConfig) (guerrilla.Daemon, error) {

	cfg := &guerrilla.AppConfig{LogFile: log.OutputStdout.String()}

	cfg.AllowedHosts = []string{"."}

	sc := guerrilla.ServerConfig{
		ListenInterface: smtpConfig.smtpListen,
		IsEnabled:       true,
	}
	cfg.Servers = append(cfg.Servers, sc)

	bcfg := backends.BackendConfig{
		"save_workers_size":  3,
		"save_process":       "HeadersParser|Header|Hasher|TelegramBot",
		"log_received_mails": true,
		"primary_mail_host":  smtpConfig.smtpPrimaryHost,
	}
	cfg.BackendConfig = bcfg

	daemon := guerrilla.Daemon{Config: cfg}
	daemon.AddProcessor("TelegramBot", TelegramBotProcessorFactory(telegramConfig))

	err := daemon.Start()
	return daemon, err
}

func TelegramBotProcessorFactory(
	telegramConfig *TelegramConfig) func() backends.Decorator {
	return func() backends.Decorator {
		// https://github.com/flashmob/go-guerrilla/wiki/Backends,-configuring-and-extending

		return func(p backends.Processor) backends.Processor {
			return backends.ProcessWith(
				func(e *mail.Envelope, task backends.SelectTask) (backends.Result, error) {
					if task == backends.TaskSaveMail {
						err := SendEmailToTelegram(e, telegramConfig)
						if err != nil {
							return backends.NewResult(fmt.Sprintf("554 Error: %s", err)), err
						}
						return p.Process(e, task)
					}
					return p.Process(e, task)
				},
			)
		}
	}
}

func SendEmailToTelegram(e *mail.Envelope,
	telegramConfig *TelegramConfig) error {

	message := FormatEmail(e, telegramConfig.messageTemplate)
	if strings.HasSuffix(MapAddresses(e.RcptTo), "@tg.com") == true {
		// socialName := "tg"
		telegramChatIds := telegramConfig.telegramChatIds + "," + FormatTgChatId(e)
		for _, chatId := range strings.Split(telegramChatIds, ",") {

			// Apparently the native golang's http client supports
			// http, https and socks5 proxies via HTTP_PROXY/HTTPS_PROXY env vars
			// out of the box.
			//
			// See: https://golang.org/pkg/net/http/#ProxyFromEnvironment
			
				resp, err := http.PostForm(
					fmt.Sprintf(
						"%sbot%s/sendMessage?disable_web_page_preview=true",
						telegramConfig.telegramApiPrefix,
						telegramConfig.telegramBotToken,
					),
					url.Values{"chat_id": {chatId}, "text": {message}},
				)
				if err != nil {
					return errors.New(SanitizeBotToken(err.Error(), telegramConfig.telegramBotToken))
				}
				if resp.StatusCode != 200 {
					body, _ := ioutil.ReadAll(resp.Body)
					return errors.New(fmt.Sprintf(
						"Non-200 response from Telegram: (%d) %s",
						resp.StatusCode,
						SanitizeBotToken(EscapeMultiLine(body), telegramConfig.telegramBotToken),
					))
				}			

		}


		fmt.Println("email is @tg.com")
	} else if strings.HasSuffix(MapAddresses(e.RcptTo), "@vk.com") == true {
		// socialName := "vk"
		telegramChatIds := telegramConfig.vkChatIds + "," + FormatVKChatId(e)
		for _, chatId := range strings.Split(telegramChatIds, ",") {

			// Apparently the native golang's http client supports
			// http, https and socks5 proxies via HTTP_PROXY/HTTPS_PROXY env vars
			// out of the box.
			//
			// See: https://golang.org/pkg/net/http/#ProxyFromEnvironment
				resp, err := http.PostForm(
					fmt.Sprintf(
						"%s/method/%s",
						telegramConfig.vkApiPrefix,
						"messages.send",
					),
					url.Values{"peer_id": {chatId},
					"message": {message},
					"access_token": {telegramConfig.vkBotToken},
					"v":            {"5.67"}},
				)
				if err != nil {
					return errors.New(SanitizeBotToken(err.Error(), telegramConfig.vkBotToken))
				}
				if resp.StatusCode != 200 {
					body, _ := ioutil.ReadAll(resp.Body)
					return errors.New(fmt.Sprintf(
						"Non-200 response from Vk: (%d) %s",
						resp.StatusCode,
						SanitizeBotToken(EscapeMultiLine(body), telegramConfig.vkBotToken),
					))
				}

	

		}

        fmt.Println("email is @vk.com")

    } else {
		message := "Not valid email adress"
		telegramChatIds := telegramConfig.telegramChatIds
		for _, chatId := range strings.Split(telegramChatIds, ",") {

			// Apparently the native golang's http client supports
			// http, https and socks5 proxies via HTTP_PROXY/HTTPS_PROXY env vars
			// out of the box.
			//
			// See: https://golang.org/pkg/net/http/#ProxyFromEnvironment
			
				resp, err := http.PostForm(
					fmt.Sprintf(
						"%sbot%s/sendMessage?disable_web_page_preview=true",
						telegramConfig.telegramApiPrefix,
						telegramConfig.telegramBotToken,
					),
					url.Values{"chat_id": {chatId}, "text": {message}},
				)
				if err != nil {
					return errors.New(SanitizeBotToken(err.Error(), telegramConfig.telegramBotToken))
				}
				if resp.StatusCode != 200 {
					body, _ := ioutil.ReadAll(resp.Body)
					return errors.New(fmt.Sprintf(
						"Non-200 response from Telegram: (%d) %s",
						resp.StatusCode,
						SanitizeBotToken(EscapeMultiLine(body), telegramConfig.telegramBotToken),
					))
				}			

		}		
        fmt.Println("Not valid email adress")
    }

	return nil
}

func FormatEmail(e *mail.Envelope, messageTemplate string) string {
	reader := e.NewReader()
	env, err := enmime.ReadEnvelope(reader)
	if err != nil {
		return fmt.Sprintf("%s\n\nError occurred during email parsing: %s", e, err)
	}
	text := env.Text
	if text == "" {
		text = e.Data.String()
	}
	r := strings.NewReplacer(
		"\\n", "\n",
		"{from}", e.MailFrom.String(),
		"{to}", MapAddresses(e.RcptTo),
		"{subject}", env.GetHeader("subject"),
		"{body}", FormatTgChatId(e) + text,
	)
	return r.Replace(messageTemplate)
}

func FormatTgChatId(e *mail.Envelope) string {
	return strings.TrimRight(MapAddresses(e.RcptTo), "@tg.com")
}
func FormatVKChatId(e *mail.Envelope) string {
	return strings.TrimRight(MapAddresses(e.RcptTo), "@vk.com")
}

func MapAddresses(a []mail.Address) string {
	s := []string{}
	for _, aa := range a {
		s = append(s, aa.String())
	}
	return strings.Join(s, ", ")
}

func EscapeMultiLine(b []byte) string {
	// Apparently errors returned by smtp must not contain newlines,
	// otherwise the data after the first newline is not getting
	// to the parsed message.
	s := string(b)
	s = strings.Replace(s, "\r", "\\r", -1)
	s = strings.Replace(s, "\n", "\\n", -1)
	return s
}

func SanitizeBotToken(s string, botToken string) string {
	return strings.Replace(s, botToken, "***", -1)
}

func sigHandler(d guerrilla.Daemon) {
	signalChannel := make(chan os.Signal, 1)

	signal.Notify(signalChannel,
		syscall.SIGTERM,
		syscall.SIGQUIT,
		syscall.SIGINT,
		syscall.SIGKILL,
		os.Kill,
	)
	for range signalChannel {
		d.Log().Infof("Shutdown signal caught")
		go func() {
			select {
			// exit if graceful shutdown not finished in 60 sec.
			case <-time.After(time.Second * 60):
				d.Log().Error("graceful shutdown timed out")
				os.Exit(1)
			}
		}()
		d.Shutdown()
		d.Log().Infof("Shutdown completed, exiting.")
		return
	}
}
