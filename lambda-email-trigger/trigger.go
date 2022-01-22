package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/lambda"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/jhillyerd/enmime"
)

var (
	lambdaName   = flag.String("lambda-name", "", "Name of lambda function")
	bucketName   = flag.String("bucket", "", "Email bucket")
	bucketPrefix = flag.String("bucket-prefix", "email", "Bucket msg prefix")
	emailID      = flag.String("email-id", "", "Email id")

	defaultRegion = "us-east-1"
)

func main() {
	flag.Parse()

	if *emailID == "" {
		log.Fatalf("-email-id is required")
	}
	if *lambdaName == "" {
		log.Fatalf("-lambda-name is required")
	}
	if *lambdaName == "" {
		log.Fatalf("-bucket is required")
	}

	region := os.Getenv("AWS_DEFAULT_REGION")
	if region == "" {
		region = defaultRegion
	}

	awsSession := session.New(&aws.Config{
		Region: &region,
	})

	s3Client := s3.New(awsSession)
	lambdaClient := lambda.New(awsSession)

	s3Path := path.Join(*bucketPrefix, *emailID)

	obj, err := s3Client.GetObject(&s3.GetObjectInput{
		Bucket: bucketName,
		Key:    &s3Path,
	})
	if err != nil {
		log.Fatalf("Failed to fetch s3://%s/%s: %s", *bucketName, s3Path, err)
	}

	r := io.LimitReader(obj.Body, 1<<22)
	env, err := enmime.ReadEnvelope(r)
	if err != nil {
		log.Fatalf("Read email err: %s", err)
	}

	rec := events.SimpleEmailRecord{
		SES: events.SimpleEmailService{
			Mail: events.SimpleEmailMessage{
				MessageID: *emailID,
				CommonHeaders: events.SimpleEmailCommonHeaders{
					Subject:   env.Root.Header.Get("Subject"),
					From:      []string{env.Root.Header.Get("From")},
					To:        []string{env.Root.Header.Get("To")},
					MessageID: env.Root.Header.Get("Message-ID"),
				},
			},
			Receipt: events.SimpleEmailReceipt{
				Recipients: []string{env.Root.Header.Get("To")},
				DKIMVerdict: events.SimpleEmailVerdict{
					Status: "PASS",
				},
				SpamVerdict: events.SimpleEmailVerdict{
					Status: "PASS",
				},
				SPFVerdict: events.SimpleEmailVerdict{
					Status: "PASS",
				},
				VirusVerdict: events.SimpleEmailVerdict{
					Status: "PASS",
				},
			},
		},
	}

	evt := events.SimpleEmailEvent{
		Records: []events.SimpleEmailRecord{rec},
	}

	payload, err := json.MarshalIndent(evt, "", "  ")
	if err != nil {
		panic(err)
	}
	log.Printf("payload: %s\n", payload)

	out, err := lambdaClient.Invoke(&lambda.InvokeInput{
		FunctionName: lambdaName,
		Payload:      payload,
	})

	if err != nil {
		log.Fatalf("Invoke err: %s", err)
	}

	var (
		code      int64
		logResult string
	)

	if out.StatusCode != nil {
		code = *out.StatusCode
	}

	if out.LogResult != nil {
		logResult = *out.LogResult
	}

	fmt.Printf("Resp: %d\n%s\n%s\n", code, out.Payload, logResult)
}
