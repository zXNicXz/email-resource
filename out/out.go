package out

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/smtp"
	"os"
	"path/filepath"
	"strings"
	"time"
)

//Execute - provides out capability
func Execute(sourceRoot, version string, input []byte) (string, error) {
	var buildTokens = map[string]string{
		"${BUILD_ID}":            os.Getenv("BUILD_ID"),
		"${BUILD_NAME}":          os.Getenv("BUILD_NAME"),
		"${BUILD_JOB_NAME}":      os.Getenv("BUILD_JOB_NAME"),
		"${BUILD_PIPELINE_NAME}": os.Getenv("BUILD_PIPELINE_NAME"),
		"${ATC_EXTERNAL_URL}":    os.Getenv("ATC_EXTERNAL_URL"),
		"${BUILD_TEAM_NAME}":     os.Getenv("BUILD_TEAM_NAME"),
	}

	if sourceRoot == "" {
		return "", errors.New("expected path to build sources as first argument")
	}

	var indata Input

	err := json.Unmarshal(input, &indata)
	if err != nil {
		return "", err
	}

	if indata.Source.SMTP.Host == "" {
		return "", errors.New(`missing required field "source.smtp.host"`)
	}

	if indata.Source.SMTP.Port == "" {
		return "", errors.New(`missing required field "source.smtp.port"`)
	}

	if indata.Source.From == "" {
		return "", errors.New(`missing required field "source.from"`)
	}

	if len(indata.Source.To) == 0 && len(indata.Params.To) == 0 {
		return "", errors.New(`missing required field "source.to" or "params.to". Must specify at least one`)
	}

	if indata.Params.Subject == "" && indata.Params.SubjectText == "" {
		return "", errors.New(`missing required field "params.subject" or "params.subject_text". Must specify at least one`)
	}

	if indata.Source.SMTP.Anonymous == false {
		if indata.Source.SMTP.Username == "" {
			return "", errors.New(`missing required field "source.smtp.username" if anonymous specify anonymous: true`)
		}

		if indata.Source.SMTP.Password == "" {
			return "", errors.New(`missing required field "source.smtp.password" if anonymous specify anonymous: true`)
		}
	}

	replaceTokens := func(sourceString string) string {
		for k, v := range buildTokens {
			sourceString = strings.Replace(sourceString, k, v, -1)
		}
		return sourceString
	}

	readSource := func(sourcePath string) (string, error) {
		if !filepath.IsAbs(sourcePath) {
			sourcePath = filepath.Join(sourceRoot, sourcePath)
		}
		var bytes []byte
		bytes, err = ioutil.ReadFile(sourcePath)
		return replaceTokens(string(bytes)), err
	}

	fromTextOrFile := func(text string, filePath string) (string, error) {
		if text != "" {
			return replaceTokens(text), nil

		}
		if filePath != "" {
			return readSource(filePath)
		}
		return "", nil
	}

	subject, err := fromTextOrFile(indata.Params.SubjectText, indata.Params.Subject)
	if err != nil {
		return "", err
	}
	subject = strings.Trim(subject, "\n")

	var headers string
	if indata.Params.Headers != "" {
		headers, err = readSource(indata.Params.Headers)
		if err != nil {
			fmt.Fprintf(os.Stderr, err.Error())
			os.Exit(1)
		}
		headers = strings.Trim(headers, "\n")
	}

	body, err := fromTextOrFile(indata.Params.BodyText, indata.Params.Body)
	if err != nil {
		return "", err
	}

	recipientsTo, err := fromTextOrFile(indata.Params.ToText, indata.Params.To)
	if err != nil {
		return "", err
	}
	if recipientsTo != "" {
		if len(recipientsTo) > 0 {
			toListArray := strings.Split(recipientsTo, ",")
			for _, toAddress := range toListArray {
				indata.Source.To = append(indata.Source.To, strings.TrimSpace(toAddress))
			}
		}
	}

	var outdata Output
	outdata.Version.Time = time.Now().UTC()
	outdata.Metadata = []MetadataItem{
		{Name: "smtp_host", Value: indata.Source.SMTP.Host},
		{Name: "subject", Value: subject},
		{Name: "version", Value: version},
	}
	outbytes, err := json.Marshal(outdata)
	if err != nil {
		return "", err
	}

	if indata.Params.SendEmptyBody == false && len(body) == 0 {
		fmt.Fprintf(os.Stderr, "Message not sent because the message body is empty and send_empty_body parameter was set to false. Github readme: https://github.com/pivotal-cf/email-resource")
		return string(outbytes), nil
	}
	var messageData []byte
	messageData = append(messageData, []byte("To: "+strings.Join(indata.Source.To, ", ")+"\n")...)
	messageData = append(messageData, []byte("From: "+indata.Source.From+"\n")...)
	if headers != "" {
		messageData = append(messageData, []byte(headers+"\n")...)
	}
	messageData = append(messageData, []byte("Subject: "+subject+"\n")...)

	messageData = append(messageData, []byte("\n")...)
	messageData = append(messageData, []byte(body)...)

	var c *smtp.Client
	var wc io.WriteCloser
	c, err = smtp.Dial(fmt.Sprintf("%s:%s", indata.Source.SMTP.Host, indata.Source.SMTP.Port))
	if err != nil {
		return "", err
	}
	defer c.Close()

	if !indata.Source.SMTP.Anonymous {
		auth := smtp.PlainAuth(
			"",
			indata.Source.SMTP.Username,
			indata.Source.SMTP.Password,
			indata.Source.SMTP.Host,
		)
		if err = c.Hello("localhost"); err != nil {
			return "", err
		}
		if ok, _ := c.Extension("STARTTLS"); ok {
			config := &tls.Config{
				ServerName:         indata.Source.SMTP.Host,
				InsecureSkipVerify: indata.Source.SMTP.SkipSSLValidation,
			}
			if err = c.StartTLS(config); err != nil {
				return "", err
			}
		}
		if auth != nil {
			if ok, _ := c.Extension("AUTH"); ok {
				if err = c.Auth(auth); err != nil {
					return "", err
				}
			}
		}
	}
	if err = c.Mail(indata.Source.From); err != nil {
		return "", err
	}
	for _, addr := range indata.Source.To {
		if err = c.Rcpt(addr); err != nil {
			return "", err
		}
	}
	wc, err = c.Data()
	if err != nil {
		return "", err
	}
	_, err = wc.Write(messageData)
	if err != nil {
		return "", err
	}
	err = wc.Close()
	if err != nil {
		return "", err
	}
	err = c.Quit()
	if err != nil {
		return "", err
	}

	return string(outbytes), err
}
