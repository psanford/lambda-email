package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	gomail "net/mail"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/aws/aws-sdk-go/service/ses"
	"github.com/aws/aws-sdk-go/service/sns"
	"github.com/inconshreveable/log15"
	"github.com/jhillyerd/enmime"
	"github.com/psanford/lambda-email/snsmsg"
)

// Nomenclature
// Third Party - external sender
// Proxy Addr  - @my-ses-email-domain.com address
// Private Account - upstream mailbox account (gmail,outlook,etc)

var (
	replaceRegex = regexp.MustCompile("[^a-zA-Z0-9]+")

	awsMailerDaemon = "mailer-daemon@amazonses.com"
)

var conf *Config

func Handler(sse events.SimpleEmailEvent) error {
	handler := log15.StreamHandler(os.Stdout, log15.LogfmtFormat())
	log15.Root().SetHandler(handler)

	if conf == nil {
		conf = loadConfig()
		initAWS()
	}

	var errors []error
	for _, record := range sse.Records {
		var (
			mail         = record.SES.Mail
			receipt      = record.SES.Receipt
			subject      = mail.CommonHeaders.Subject
			toHeader     = mail.CommonHeaders.To
			originalFrom = mail.CommonHeaders.From

			fromAddr   string
			toOutbound bool
		)

		lgr := log15.New("msg_id", mail.MessageID, "from", originalFrom, "to", toHeader, "subject", subject, "spam", receipt.SpamVerdict.Status, "dkim", receipt.DKIMVerdict.Status, "spf", receipt.SPFVerdict.Status, "virus", receipt.VirusVerdict.Status)

		type suspectReason struct {
			name   string
			status string
		}

		var (
			virus   bool
			suspect []suspectReason
		)

		if receipt.DKIMVerdict.Status != "PASS" {
			suspect = append(suspect, suspectReason{
				name:   "dkim_verdict",
				status: receipt.DKIMVerdict.Status,
			})
		}
		if receipt.SPFVerdict.Status != "PASS" {
			suspect = append(suspect, suspectReason{
				name:   "spf_verdict",
				status: receipt.DKIMVerdict.Status,
			})
		}
		if receipt.VirusVerdict.Status != "PASS" {
			suspect = append(suspect, suspectReason{
				name:   "virus_verdict",
				status: receipt.DKIMVerdict.Status,
			})
			virus = true
		}

		lgr.Info("got_message", "suspect", suspect)

		if virus {
			lgr.Info("virus_email_not_processing")
			err := sendErrorEmail("Virus email, not processing", record)
			if err != nil {
				lgr.Error("sendError err", "err", err)
			}
			return nil
		}

		if len(originalFrom) > 0 {
			if addr, err := gomail.ParseAddress(originalFrom[0]); err == nil {
				fromAddr = addr.Address
			}
		}

		if strings.ToLower(fromAddr) == awsMailerDaemon {
			lgr.Info("mailer_daemon_notification_not_forwading")
			err := sendErrorEmail("aws mailer-daemon notification, not forwarding", record)
			if err != nil {
				lgr.Error("sendError err", "err", err)
			}
			return nil
		}

		for _, toAddr := range toHeader {
			if addr, err := gomail.ParseAddress(toAddr); err == nil {
				if addr.Address == conf.OutboundAddress {
					toOutbound = true
				}
			}
		}

		var skipForwarding bool

		for _, rule := range conf.Routes {
			match, err := rule.Match(toHeader, fromAddr)
			if err != nil {
				lgr.Error("match_rule_err", "err", err)
				continue
			}
			if match {
				if rule.Drop {
					lgr.Info("matched_drop_rule")
					return nil
				}

				if len(suspect) > 0 && !rule.AllowSuspectMessages {
					lgr.Error("matched_rule_but_suspect", "rule", rule, "suspect", subject)
					return fmt.Errorf("matched_rule_but_suspect %s", mail.MessageID)
				}

				lgr.Info("publish_sns_route", "sns_topic", rule.SNS)
				err = publishSNS(rule.SNS, record)
				if err != nil {
					lgr.Error("publish_sns_err", "err", err)
					return err
				}
				if !rule.Forward {
					skipForwarding = true
				}
			}
		}

		if !skipForwarding {
			if fromAddr == conf.PrivateAccountAddress {
				if toOutbound {
					if err := handleOutbound(record); err != nil {
						lgr.Error("handle_outbound_err", "err", err)
						errors = append(errors, err)
					}
				} else {
					if err := handleReply(lgr, record); err != nil {
						lgr.Error("handle_reply_err", "err", err)
						errors = append(errors, err)
					}
				}
			} else {
				if err := forwardToGmail(record); err != nil {
					lgr.Error("forward_to_gmail_err", "err", err)
					errors = append(errors, err)
				}
			}
		} else {
			lgr.Info("skip_forwarding")
		}
	}

	if len(errors) > 0 {
		return fmt.Errorf("%+v", errors)
	}

	return nil
}

func handleOutbound(record events.SimpleEmailRecord) error {
	src := path.Join(conf.Bucket.Name, conf.Bucket.MsgPrefix, record.SES.Mail.MessageID)
	dst := path.Join(conf.Bucket.OutboxPrefix, record.SES.Mail.MessageID)
	copyReq := s3.CopyObjectInput{
		Bucket:     &conf.Bucket.Name,
		CopySource: &src,
		Key:        &dst,
	}
	_, err := s3CopyObj(&copyReq)
	return err
}

func sendErrorEmail(msg string, record events.SimpleEmailRecord) error {
	b := enmime.Builder()
	fromAddr := "error@" + conf.Domain
	b = b.From("Lambda Email Error", fromAddr)
	forwardToAddr := conf.PrivateAccountMailbox() + "@" + conf.PrivateAccountDomain()
	b = b.To("", forwardToAddr)
	b = b.Subject("Lambda Email Error")

	payload, _ := json.MarshalIndent(record, "", "  ")

	body := msg + "\nid: " + record.SES.Mail.MessageID + "\n\n" + string(payload)
	b = b.Text([]byte(body))

	root, err := b.Build()
	if err != nil {
		return fmt.Errorf("Build errormsg email err=%q", err)
	}

	var buf bytes.Buffer
	if err := root.Encode(&buf); err != nil {
		return fmt.Errorf("Encode errormsg email err=%q", err)
	}

	sendEmailInput := &ses.SendRawEmailInput{
		Destinations: strList([]string{forwardToAddr}),
		RawMessage: &ses.RawMessage{
			Data: buf.Bytes(),
		},
		Source: &fromAddr,
	}

	_, err = sendEmail(sendEmailInput)
	if err != nil {
		return fmt.Errorf("send errormsg email error: %s", err)
	}

	return nil
}

func forwardToGmail(record events.SimpleEmailRecord) error {
	var (
		forwardToAddr      string
		substituteFromAddr string
		substituteFromName string

		mail         = record.SES.Mail
		subject      = mail.CommonHeaders.Subject
		toHeader     = mail.CommonHeaders.To
		originalFrom = mail.CommonHeaders.From
	)

	for _, recipient := range record.SES.Receipt.Recipients {
		recipient = strings.ToLower(recipient)
		parts := strings.SplitN(recipient, "@", 2)
		if len(parts) < 2 {
			continue
		}

		if parts[1] == conf.Domain {
			sanitized := replaceRegex.ReplaceAllString(parts[0], "_")
			forwardToAddr = conf.PrivateAccountMailbox() + "+" + sanitized + "@" + conf.PrivateAccountDomain()
			substituteFromAddr = recipient
			break
		}
	}

	if forwardToAddr == "" {
		return fmt.Errorf("Failed to find %s address for email %s", conf.Domain, mail.MessageID)
	}

	if len(originalFrom) > 0 {
		if addr, err := gomail.ParseAddress(originalFrom[0]); err == nil && addr.Name != "" {
			substituteFromName = addr.Name
		}
	}

	msgReader, err := getMessage(mail.MessageID)
	if err != nil {
		return fmt.Errorf("GetMessage err=%q", err)
	}

	body, err := enmime.ReadEnvelope(msgReader)
	if err != nil {
		return fmt.Errorf("Parse email err=%q", err)
	}

	b := enmime.Builder()
	b = b.From(substituteFromName, substituteFromAddr)
	b = b.To("", forwardToAddr)
	b = b.Subject(subject)
	if len(body.Text) > 0 {
		b = b.Text([]byte(body.Text))
	}
	if len(body.HTML) > 0 {
		b = b.HTML([]byte(body.HTML))
	}

	for _, p := range body.Attachments {
		if len(p.Content) > 0 {
			b = b.AddAttachment(p.Content, p.ContentType, p.FileName)
		}
	}

	for _, p := range body.Inlines {
		if len(p.Content) > 0 {
			b = b.AddInline(p.Content, p.ContentType, p.FileName, p.ContentID)
		}
	}

	for _, p := range body.OtherParts {
		if len(p.Content) > 0 {
			b = b.AddInline(p.Content, p.ContentType, p.FileName, p.ContentID)
		}
	}

	hasAttachments := len(body.Attachments) > 0 || len(body.Inlines) > 0 || len(body.OtherParts) > 0
	hasOtherAttachments := len(body.OtherParts) > 0

	b = b.Header("X-Lambdaemail-Date", mail.Timestamp.String())
	b = b.Header("X-Lambdaemail-From", strings.Join(originalFrom, ","))
	b = b.Header("X-Lambdaemail-To", strings.Join(toHeader, ","))
	b = b.Header("X-Lambdaemail-Id", mail.MessageID)
	b = b.Header("X-Lambdaemail-Has-Attachments", strconv.FormatBool(hasAttachments))
	b = b.Header("X-Lambdaemail-Has-Other-Attachments", strconv.FormatBool(hasOtherAttachments))

	root, err := b.Build()
	if err != nil {
		return fmt.Errorf("Build forward email err=%q", err)
	}

	var buf bytes.Buffer
	if err := root.Encode(&buf); err != nil {
		return fmt.Errorf("Encode forward email err=%q", err)
	}

	sendEmailInput := &ses.SendRawEmailInput{
		Destinations: strList([]string{forwardToAddr}),
		RawMessage: &ses.RawMessage{
			Data: buf.Bytes(),
		},
		Source: &substituteFromAddr,
	}

	sendResult, err := sendEmail(sendEmailInput)
	if err != nil {
		return fmt.Errorf("send email error: %s", err)
	}

	if sendResult.MessageId != nil {
		origMsgID := mail.CommonHeaders.MessageID
		origMsgID = strings.TrimLeft(origMsgID, "<")
		origMsgID = strings.TrimRight(origMsgID, ">")

		msg := forwardInfo{
			OriginalMessageID: origMsgID,
			SESID:             mail.MessageID,
			ForwardedID:       *sendResult.MessageId,
		}

		err = putForwardInfo(msg)
		if err != nil {
			return fmt.Errorf("save forward info error: %s", err)
		}
	}

	return nil
}

func handleReply(lgr log15.Logger, record events.SimpleEmailRecord) error {
	// lookup what we are replying to
	// fetch original message
	// get from address from that message
	// reply from our @proxyaddr address to that message

	lgr = lgr.New()

	var (
		mail = record.SES.Mail

		originalFrom = mail.CommonHeaders.From
		subject      = mail.CommonHeaders.Subject

		proxyAddr          string
		substituteFromName string
	)

	for _, recipient := range record.SES.Receipt.Recipients {
		recipient = strings.ToLower(recipient)
		parts := strings.SplitN(recipient, "@", 2)
		if len(parts) < 2 {
			continue
		}

		if parts[1] == conf.Domain {
			proxyAddr = recipient
			break
		}
	}

	if proxyAddr == "" {
		return fmt.Errorf("Failed to find %s address for email %s", conf.Domain, mail.MessageID)
	}

	if len(originalFrom) > 0 {
		if addr, err := gomail.ParseAddress(originalFrom[0]); err == nil && addr.Name != "" {
			substituteFromName = addr.Name
		}
	}

	msgReader, err := getMessage(mail.MessageID)
	if err != nil {
		return fmt.Errorf("GetMessage err=%q", err)
	}

	body, err := enmime.ReadEnvelope(msgReader)
	if err != nil {
		return fmt.Errorf("Parse email err=%q", err)
	}

	inReplyTo := trimBrackets(body.GetHeader("In-Reply-To"))
	if inReplyTo == "" {
		return fmt.Errorf("No in-reply-to header found")
	}

	replyToId := strings.TrimSuffix(inReplyTo, "@email.amazonses.com")

	info, err := getForwardInfo(replyToId)
	if err != nil {
		return fmt.Errorf("GetForwardInfo replyToId=%s err=%q", replyToId, err)
	}

	originalMsgReader, err := getMessage(info.SESID)
	if err != nil {
		return fmt.Errorf("GetMessage (original) err=%q", err)
	}
	origBody, err := enmime.ReadEnvelope(originalMsgReader)
	if err != nil {
		return fmt.Errorf("Parse email (original) err=%q", err)
	}

	replyToStr := origBody.GetHeader("reply-to")
	if replyToStr == "" {
		replyToStr = origBody.GetHeader("from")
	}

	replyTo, err := gomail.ParseAddress(replyToStr)
	if err != nil {
		return fmt.Errorf("Parse reply-to err=%q", err)
	}

	b := enmime.Builder()
	b = b.From(substituteFromName, proxyAddr)
	b = b.To(replyTo.Name, replyTo.Address)
	b = b.Subject(subject)
	b = b.Header("In-Reply-To", "<"+inReplyTo+">")
	b = b.Header("References", "<"+inReplyTo+">")

	if len(body.Text) > 0 {
		b = b.Text([]byte(body.Text))
	}
	if len(body.HTML) > 0 {
		b = b.HTML([]byte(body.HTML))
	}

	for _, p := range body.Attachments {
		b = b.AddAttachment(p.Content, p.ContentType, p.FileName)
	}

	for _, p := range body.Inlines {
		b = b.AddInline(p.Content, p.ContentType, p.FileName, p.ContentID)
	}

	for _, p := range body.OtherParts {
		b = b.AddInline(p.Content, p.ContentType, p.FileName, p.ContentID)
	}

	root, err := b.Build()
	if err != nil {
		return fmt.Errorf("Build forward email err=%q", err)
	}

	var buf bytes.Buffer
	if err := root.Encode(&buf); err != nil {
		return fmt.Errorf("Encode forward email err=%q", err)
	}

	sendEmailInput := &ses.SendRawEmailInput{
		Destinations: strList([]string{replyTo.Address}),
		RawMessage: &ses.RawMessage{
			Data: buf.Bytes(),
		},
		Source: &proxyAddr,
	}

	sendResult, err := sendEmail(sendEmailInput)
	if err != nil {
		return fmt.Errorf("send email error: %s", err)
	}

	lgr.Info("replied_message", "id", *sendResult.MessageId, "in_reply_to", inReplyTo, "to", replyTo.Address, "from", proxyAddr)

	return nil
}

func publishSNS(topic string, record events.SimpleEmailRecord) error {
	id := record.SES.Mail.MessageID
	url, err := presignMessage(id)
	if err != nil {
		return fmt.Errorf("presign url for %s err: %w", id, err)
	}

	msg := snsmsg.Msg{
		ID:           record.SES.Mail.MessageID,
		From:         record.SES.Mail.CommonHeaders.From,
		To:           record.SES.Mail.CommonHeaders.To,
		Subject:      record.SES.Mail.CommonHeaders.Subject,
		Date:         record.SES.Mail.CommonHeaders.Date,
		PresignedURL: url,
	}

	payloadBytes, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal sqs msg err: %w", err)
	}

	payload := string(payloadBytes)

	_, err = snsPublish(&sns.PublishInput{
		Message:  &payload,
		TopicArn: &topic,
	})

	if err != nil {
		return fmt.Errorf("snsPublish err for %s: %w", id, err)
	}

	return nil
}

var (
	sendEmail   func(*ses.SendRawEmailInput) (*ses.SendRawEmailOutput, error)
	s3GetObj    func(*s3.GetObjectInput) (*s3.GetObjectOutput, error)
	s3PutObj    func(*s3manager.UploadInput, ...func(*s3manager.Uploader)) (*s3manager.UploadOutput, error)
	s3CopyObj   func(*s3.CopyObjectInput) (*s3.CopyObjectOutput, error)
	s3GetObjReq func(*s3.GetObjectInput) (*request.Request, *s3.GetObjectOutput)

	snsPublish func(*sns.PublishInput) (*sns.PublishOutput, error)
)

func trimBrackets(s string) string {
	return strings.Trim(s, "<>")
}

func initAWS() {
	awsSession := session.New(&aws.Config{
		Region: &conf.AwsRegion,
	})
	s3Client := s3.New(awsSession)
	s3Uploader := s3manager.NewUploader(awsSession)
	sesClient := ses.New(awsSession)
	snsClient := sns.New(awsSession)

	sendEmail = sesClient.SendRawEmail
	s3GetObj = s3Client.GetObject
	s3PutObj = s3Uploader.Upload
	s3CopyObj = s3Client.CopyObject
	s3GetObjReq = s3Client.GetObjectRequest
	snsPublish = snsClient.Publish
}

func getMessage(id string) (io.Reader, error) {
	p := path.Join(conf.Bucket.MsgPrefix, id)
	getObj := &s3.GetObjectInput{
		Bucket: &conf.Bucket.Name,
		Key:    &p,
	}
	obj, err := s3GetObj(getObj)
	if err != nil {
		return nil, err
	}

	return obj.Body, nil
}

func presignMessage(id string) (string, error) {
	p := path.Join(conf.Bucket.MsgPrefix, id)
	getObj := &s3.GetObjectInput{
		Bucket: &conf.Bucket.Name,
		Key:    &p,
	}

	req, _ := s3GetObjReq(getObj)

	return req.Presign(5 * time.Minute)
}

func getForwardInfo(id string) (forwardInfo, error) {
	var info forwardInfo

	p := path.Join(conf.Bucket.ForwardMetaPrefix, id)
	getObj := &s3.GetObjectInput{
		Bucket: &conf.Bucket.Name,
		Key:    &p,
	}
	obj, err := s3GetObj(getObj)
	if err != nil {
		return info, err
	}
	defer obj.Body.Close()

	dec := json.NewDecoder(obj.Body)

	err = dec.Decode(&info)
	return info, err
}

type forwardInfo struct {
	OriginalMessageID string `json:"original_message_id"`
	SESID             string `json:"ses_id"`
	ForwardedID       string `json:"forwarded_id"`
}

func putForwardInfo(msg forwardInfo) error {
	p := path.Join(conf.Bucket.ForwardMetaPrefix, msg.ForwardedID)

	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("JSON marshal error: %s", err)
	}

	uinput := &s3manager.UploadInput{
		Bucket: &conf.Bucket.Name,
		Key:    &p,
		Body:   bytes.NewReader(data),
	}

	_, err = s3PutObj(uinput)
	return err
}

func putPayload(sse events.SimpleEmailEvent) error {
	p := fmt.Sprintf("%s/sse-%s", conf.Bucket.ForwardMetaPrefix, time.Now().Format(time.RFC3339))

	data, err := json.Marshal(sse)
	if err != nil {
		return fmt.Errorf("JSON marshal error: %s", err)
	}

	uinput := &s3manager.UploadInput{
		Bucket: &conf.Bucket.Name,
		Key:    &p,
		Body:   bytes.NewReader(data),
	}

	_, err = s3PutObj(uinput)
	return err
}

func strList(strs []string) []*string {
	if strs == nil {
		return nil
	}

	ret := make([]*string, len(strs))
	for i := range strs {
		trimmed := strings.TrimSpace(strs[i])
		ret[i] = &trimmed
	}
	return ret
}

func main() {
	lambda.Start(Handler)
}

const privateAddrPlaceholder = "__PRIVATE_ADDRESS__"

func (r *Route) Match(toHeader []string, from string) (bool, error) {
	// TODO(PMS):
	// - match other things like subject and body

	src := r.Src
	dst := r.Dst
	if src == privateAddrPlaceholder {
		src = conf.PrivateAccountAddress
	}

	if dst == privateAddrPlaceholder {
		dst = conf.PrivateAccountAddress
	}

	var (
		srcRe *regexp.Regexp
		dstRe *regexp.Regexp
		err   error
	)

	if strings.HasPrefix(src, "/") && strings.HasSuffix(src, "/") {
		src = strings.TrimPrefix(src, "/")
		src = strings.TrimSuffix(src, "/")

		srcRe, err = regexp.Compile(src)
		if err != nil {
			return false, fmt.Errorf("src regexp compile err for %s: %w", r.Src, err)
		}
	}

	if srcRe != nil {
		if !srcRe.MatchString(from) {
			return false, nil
		}
	} else if src != from {
		return false, nil
	}

	var match bool
	for _, toAddr := range toHeader {
		if addr, err := gomail.ParseAddress(toAddr); err == nil {
			if dstRe != nil {
				if dstRe.MatchString(addr.Address) {
					match = true
					break
				}
			} else if addr.Address == dst {
				match = true
				break
			}
		}
	}

	return match, nil
}
