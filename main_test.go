package main

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path"
	"strings"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/client"
	"github.com/aws/aws-sdk-go/aws/client/metadata"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/defaults"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/aws/aws-sdk-go/service/ses"
	"github.com/aws/aws-sdk-go/service/sns"
	"github.com/go-test/deep"
	"github.com/jhillyerd/enmime"
	"github.com/psanford/lambda-email/snsmsg"
)

func TestLambdaEmail(t *testing.T) {
	conf = &Config{
		Domain:                "my-ses-email-domain.example.com",
		PrivateAccountAddress: "foo@gmail.example.com",
		Bucket: Bucket{
			Name:              "westerly-tapir",
			MsgPrefix:         "/periphery-corollas",
			ForwardMetaPrefix: "/Voldemort-wearily",
		},
		Routes: []Route{
			{
				Src:     "psanford@example.com",
				Dst:     "test@my-ses-email-domain.example.com",
				SNS:     "hornet-breakwaters",
				Forward: true,
			},
		},
	}
	sendEmail = fakeSendEmail
	s3GetObj = fakeGetObj
	s3PutObj = fakePutObj
	s3CopyObj = fakeCopyObj
	s3GetObjReq = fakeGetObjReq
	snsPublish = fakeSNSPublish

	log.SetOutput(ioutil.Discard)

	fSSE, err := os.Open("test_data/sse-metadata")
	if err != nil {
		t.Fatal(err)
	}
	defer fSSE.Close()

	dec := json.NewDecoder(fSSE)
	var sse events.SimpleEmailEvent
	err = dec.Decode(&sse)
	if err != nil {
		t.Fatal(err)
	}

	fMsg0, err := os.Open("test_data/msg0")
	if err != nil {
		t.Fatal(err)
	}
	defer fMsg0.Close()

	sesID := fmt.Sprintf("%s/%s", conf.Bucket.MsgPrefix, sse.Records[0].SES.Mail.MessageID)
	fakeMsg := s3manager.UploadInput{
		Bucket: &conf.Bucket.Name,
		Key:    &sesID,
		Body:   fMsg0,
	}

	_, err = fakePutObj(&fakeMsg)
	if err != nil {
		t.Fatal(err)
	}

	err = Handler(sse)
	if err != nil {
		t.Fatal(err)
	}

	if len(sentEmails) != 1 {
		t.Fatalf("Expected 1 send email but got %d", len(sentEmails))
	}

	sent := sentEmails[0]
	env, err := enmime.ReadEnvelope(bytes.NewReader(sent.input.RawMessage.Data))
	if err != nil {
		t.Fatal(err)
	}

	var headerChecks = []struct {
		headerName string
		expect     string
	}{
		{"X-Lambdaemail-From", "Peter Sanford <psanford@example.com>"},
		{"X-Lambdaemail-To", "test@my-ses-email-domain.example.com"},
		{"X-Lambdaemail-Id", "8ffg1s10miueo0o4qhb37ss9ilq26akqpo7pr8o1"},
	}
	for _, c := range headerChecks {
		if g, e := env.GetHeader(c.headerName), c.expect; g != e {
			t.Errorf("Header %s mismatch got:%q != expect:%q", c.headerName, g, e)
		}
	}

	metaPath := fmt.Sprintf("%s/%s", conf.Bucket.ForwardMetaPrefix, sent.sendID)
	getObj := &s3.GetObjectInput{
		Bucket: &conf.Bucket.Name,
		Key:    &metaPath,
	}

	got, err := fakeGetObj(getObj)
	if err != nil {
		t.Fatal(err)
	}

	var forwardMeta forwardInfo
	body, _ := ioutil.ReadAll(got.Body)
	err = json.Unmarshal(body, &forwardMeta)
	if err != nil {
		t.Fatal(err)
	}

	expectMeta := forwardInfo{
		OriginalMessageID: "5B762F46B550CE2BAB51989E4F9F1280@mail.gmail.com",
		SESID:             "8ffg1s10miueo0o4qhb37ss9ilq26akqpo7pr8o1",
		ForwardedID:       sent.sendID,
	}

	if diff := deep.Equal(forwardMeta, expectMeta); diff != nil {
		t.Error(diff)
	}

	if len(snsMessages) != 1 {
		log.Fatalf("Expected 1 sns publish but got %d", len(snsMessages))
	}
}

func TestRuleMatch(t *testing.T) {
	r := Route{
		Src:  "furriest@imperative.blowsy.mustachio",
		Dst:  "/.*/",
		Drop: true,
	}

	var (
		to   = "smithereens@reuses.bloodhounds"
		from = "Steve Mustachio <furriest@imperative.blowsy.mustachio>"
	)

	match, err := r.Match([]string{to}, from)
	if err != nil {
		t.Fatal(err)
	}
	if !match {
		t.Fatalf("Expected match")
	}
}

var (
	fakeS3      = make(map[bucketKey][]byte)
	sentEmails  []sentEmail
	snsMessages []snsmsg.Msg
)

type sentEmail struct {
	input  *ses.SendRawEmailInput
	sendID string
}

type bucketKey struct {
	bucket string
	key    string
}

func fakeSendEmail(i *ses.SendRawEmailInput) (*ses.SendRawEmailOutput, error) {

	id := make([]byte, 32)
	_, err := io.ReadFull(rand.Reader, id)
	if err != nil {
		panic(err)
	}

	idStr := fmt.Sprintf("%x", id)
	sent := sentEmail{
		input:  i,
		sendID: idStr,
	}

	sentEmails = append(sentEmails, sent)

	return &ses.SendRawEmailOutput{MessageId: &idStr}, nil

}

func fakeGetObj(i *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
	key := bucketKey{*i.Bucket, *i.Key}
	if obj, found := fakeS3[key]; found {
		out := &s3.GetObjectOutput{
			Body: ioutil.NopCloser(bytes.NewReader(obj)),
		}
		return out, nil
	}

	return nil, awserr.New(s3.ErrCodeNoSuchKey, s3.ErrCodeNoSuchKey, nil)
}

func fakePutObj(i *s3manager.UploadInput, o ...func(*s3manager.Uploader)) (*s3manager.UploadOutput, error) {
	b, err := ioutil.ReadAll(i.Body)
	if err != nil {
		return nil, err
	}

	key := bucketKey{*i.Bucket, *i.Key}
	fakeS3[key] = b

	return &s3manager.UploadOutput{}, nil
}

func fakeCopyObj(i *s3.CopyObjectInput) (*s3.CopyObjectOutput, error) {
	parts := strings.Split(*i.CopySource, "/")
	srcBucket := parts[0]
	srcPath := path.Join(parts[1:]...)
	src := bucketKey{srcBucket, srcPath}
	dst := bucketKey{*i.Bucket, *i.Key}

	obj, ok := fakeS3[src]
	if !ok {
		return nil, awserr.New(s3.ErrCodeNoSuchKey, s3.ErrCodeNoSuchKey, nil)
	}

	fakeS3[dst] = obj
	return &s3.CopyObjectOutput{}, nil
}

func fakeGetObjReq(*s3.GetObjectInput) (*request.Request, *s3.GetObjectOutput) {
	op := &request.Operation{}

	cfg := aws.Config{
		Region:      aws.String("foo"),
		Credentials: credentials.NewStaticCredentials("", "", ""),
	}

	md := metadata.ClientInfo{
		Endpoint: "http://127.0.0.1",
	}

	req := request.New(cfg, md, defaults.Handlers(), client.DefaultRetryer{NumMaxRetries: 2}, op, nil, nil)
	return req, nil
}

func fakeSNSPublish(i *sns.PublishInput) (*sns.PublishOutput, error) {
	var msg snsmsg.Msg

	err := json.Unmarshal([]byte(*i.Message), &msg)
	if err != nil {
		panic(err)
	}

	snsMessages = append(snsMessages, msg)
	return nil, nil
}
