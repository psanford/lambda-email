package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/mail"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/aws/aws-sdk-go/service/ses"
	"github.com/jhillyerd/enmime"
	cli "gopkg.in/urfave/cli.v1"
)

var (
	s3Client   *s3.S3
	sesClient  *ses.SES
	s3Uploader *s3manager.Uploader

	region        string
	defaultRegion = "us-east-1"
)

func main() {
	app := cli.NewApp()

	region = os.Getenv("AWS_DEFAULT_REGION")
	if region == "" {
		region = defaultRegion
	}

	app.Commands = []cli.Command{
		{
			Name:   "list",
			Usage:  "List pending outbound messages",
			Action: listMessages,
			Flags: []cli.Flag{
				cli.StringFlag{
					Name:  "bucket",
					Value: "",
					Usage: "S3 message bucket",
				},
				cli.StringFlag{
					Name:  "msg_prefix",
					Value: "/email",
					Usage: "S3 bucket message prefix",
				},
				cli.StringFlag{
					Name:  "outbox_prefix",
					Value: "/outbox",
					Usage: "S3 bucket outbox prefix",
				},
			},
		},
		{
			Name:   "get",
			Usage:  "Get a message from the outbox",
			Action: pullMessage,
			Flags: []cli.Flag{
				cli.StringFlag{
					Name:  "bucket",
					Value: "",
					Usage: "S3 message bucket",
				},
				cli.StringFlag{
					Name:  "msg_prefix",
					Value: "/email",
					Usage: "S3 bucket message prefix",
				},
				cli.StringFlag{
					Name:  "outbox_prefix",
					Value: "/outbox",
					Usage: "S3 bucket outbox prefix",
				},
			},
		},
		{
			Name:  "send",
			Usage: "Send a message",
			Flags: []cli.Flag{
				cli.StringFlag{
					Name:  "from",
					Value: "",
					Usage: "From addr (e.g. 'Peter Sanford <peter@proxy.example.com>)",
				},
				cli.StringSliceFlag{
					Name:  "to",
					Usage: "To addr",
				},
				cli.StringSliceFlag{
					Name:  "cc",
					Usage: "CC addr",
				},
				cli.StringSliceFlag{
					Name:  "bcc",
					Usage: "Bcc addr",
				},
				cli.StringFlag{
					Name:  "id",
					Value: "",
					Usage: "Message ID",
				},
				cli.StringFlag{
					Name:  "bucket",
					Value: "",
					Usage: "S3 message bucket",
				},
				cli.StringFlag{
					Name:  "msg_prefix",
					Value: "/email",
					Usage: "S3 bucket message prefix",
				},
				cli.StringFlag{
					Name:  "outbox_prefix",
					Value: "/outbox",
					Usage: "S3 bucket outbox prefix",
				},
			},
			Action: sendMessage,
		},
	}

	sort.Sort(cli.CommandsByName(app.Commands))

	awsSession := session.New(&aws.Config{
		Region: &region,
	})
	s3Client = s3.New(awsSession)
	s3Uploader = s3manager.NewUploader(awsSession)
	sesClient = ses.New(awsSession)

	err := app.Run(os.Args)
	if err != nil {
		log.Fatal(err)
	}
}

func pullMessage(c *cli.Context) error {
	bucket := c.String("bucket")
	msgPrefix := c.String("msg_prefix")

	if bucket == "" {
		return fmt.Errorf("-bucket is requred")
	}

	if msgPrefix == "" {
		return fmt.Errorf("-msg_prefix is requred")
	}

	name := c.Args().First()
	if name == "" {
		log.Fatal("must specify message id")
	}
	r, err := getMessage(bucket, msgPrefix, name)
	if err != nil {
		return err
	}

	_, err = io.Copy(os.Stdout, r)
	if err != nil {
		return err
	}
	return nil
}

func listMessages(c *cli.Context) error {
	bucket := c.String("bucket")
	msgPrefix := c.String("msg_prefix")
	outboxPrefix := c.String("outbox_prefix")

	if bucket == "" {
		return fmt.Errorf("-bucket is requred")
	}

	if msgPrefix == "" {
		return fmt.Errorf("-msg_prefix is requred")
	}

	if outboxPrefix == "" {
		return fmt.Errorf("-outbox_prefix is requred")
	}

	var obj s3.Object
	iter := listObjects(bucket, outboxPrefix+"/", &obj)

	for iter.Next() {
		_, k := path.Split(*obj.Key)
		fmt.Printf("%s %s\n", k, obj.LastModified)
	}
	err := iter.Close()
	if err != nil {
		return err
	}

	return nil
}

func sendMessage(c *cli.Context) error {
	from := c.String("from")
	tos := c.StringSlice("to")
	ccs := c.StringSlice("cc")
	bccs := c.StringSlice("bcc")
	id := c.String("id")

	bucket := c.String("bucket")
	msgPrefix := c.String("msg_prefix")
	outboxPrefix := c.String("outbox_prefix")

	if bucket == "" {
		return fmt.Errorf("-bucket is requred")
	}

	if msgPrefix == "" {
		return fmt.Errorf("-msg_prefix is requred")
	}

	if outboxPrefix == "" {
		return fmt.Errorf("-outbox_prefix is requred")
	}

	if from == "" || len(tos) < 1 || id == "" {
		return fmt.Errorf("-from -to -id are required flags")
	}

	log.Printf("Fetch %s", id)
	msgReader, err := getMessage(bucket, msgPrefix, id)
	if err != nil {
		return fmt.Errorf("GetMessage err=%q", err)
	}

	body, err := enmime.ReadEnvelope(msgReader)
	if err != nil {
		return fmt.Errorf("Parse email err=%q", err)
	}

	var destinations []string

	subject := body.GetHeader("subject")

	b := enmime.Builder()

	fromAddr, err := mail.ParseAddress(from)
	if err != nil {
		return fmt.Errorf("Parse from err: %w", err)
	}

	b = b.From(fromAddr.Name, fromAddr.Address)

	for _, to := range tos {
		b = b.To("", to)
		destinations = append(destinations, to)
	}
	for _, cc := range ccs {
		b = b.CC("", cc)
		destinations = append(destinations, cc)
	}
	for _, bcc := range bccs {
		b = b.BCC("", bcc)
		destinations = append(destinations, bcc)
	}
	b = b.Subject(subject)
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
		Destinations: strList(destinations),
		RawMessage: &ses.RawMessage{
			Data: buf.Bytes(),
		},
		Source: &from,
	}

	log.Printf("Send %s to:%v from:%s cc:%v bcc:%v", id, tos, from, ccs, bccs)
	sendResult, err := sesClient.SendRawEmail(sendEmailInput)
	if err != nil {
		return fmt.Errorf("send email error: %s", err)
	}

	if sendResult.MessageId != nil {
		log.Printf("Sent! id=%s", *sendResult.MessageId)
		sentPath := path.Join("sent", *sendResult.MessageId)
		uinput := &s3manager.UploadInput{
			Bucket: &bucket,
			Key:    &sentPath,
			Body:   bytes.NewReader(buf.Bytes()),
		}
		_, err = s3Uploader.Upload(uinput)
		if err != nil {
			return fmt.Errorf("Failed to save to sent dir: %s", err)
		}
		log.Printf("Saved to sent dir")

		outboxPath := path.Join(outboxPrefix, id)
		deleteReq := s3.DeleteObjectInput{
			Bucket: &bucket,
			Key:    &outboxPath,
		}
		_, err = s3Client.DeleteObject(&deleteReq)
		if err != nil {
			return fmt.Errorf("Failed to delete message from outbox: %s", err)
		}
		log.Printf("Deleted from outbox")
	}

	return nil
}

type objIter struct {
	bucket     string
	delimiter  string
	prefix     string
	hasMore    bool
	nextMarker *string

	dest *s3.Object

	err error

	objs []*s3.Object
}

func (i *objIter) Next() bool {
	if i.err != nil {
		return false
	}

	if len(i.objs) == 0 {
		if i.hasMore {
			input := &s3.ListObjectsInput{
				Bucket:    &i.bucket,
				Delimiter: aws.String("/"),
				Prefix:    &i.prefix,
				Marker:    i.nextMarker,
			}

			output, err := s3Client.ListObjects(input)
			if err != nil {
				i.err = err
				return false
			}

			i.objs = output.Contents
			if output.IsTruncated != nil && *output.IsTruncated {
				i.nextMarker = output.Marker
			} else {
				i.hasMore = false
			}
		}
	}

	if len(i.objs) > 0 {
		*i.dest = *i.objs[0]
		i.objs = i.objs[1:]
		return true
	}

	return false
}

func (i *objIter) Close() error {
	return i.err
}

func listObjects(bucket, path string, obj *s3.Object) *objIter {
	path = strings.TrimLeft(path, "/")
	iter := objIter{
		bucket:  bucket,
		prefix:  path,
		hasMore: true,
		dest:    obj,
	}

	return &iter
}

func getMessage(bucket, msgPrefix, id string) (io.Reader, error) {
	p := path.Join(msgPrefix, id)
	getObj := &s3.GetObjectInput{
		Bucket: &bucket,
		Key:    &p,
	}
	obj, err := s3Client.GetObject(getObj)
	if err != nil {
		return nil, err
	}

	return obj.Body, nil
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
