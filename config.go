package main

import (
	"errors"
	"io/ioutil"
	"os"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/inconshreveable/log15"
)

type Config struct {
	Domain                string `toml:"domain"`
	PrivateAccountAddress string `toml:"private_address"`

	OutboundAddress string `toml:"outbound_address"`

	SSMPrefix string `toml:"ssm_prefix"`
	AwsRegion string `toml:"aws_region"`
	Bucket    Bucket `toml:"bucket"`

	Routes []Route `toml:"route"`
}

type Route struct {
	Src                  string `toml:"src"`
	Dst                  string `toml:"dst"`
	SNS                  string `toml:"sns"`
	Forward              bool   `toml:"forward"`
	AllowSuspectMessages bool   `toml:"allow_suspect_messages"`
}

type Bucket struct {
	Name              string `toml:"name"`
	MsgPrefix         string `toml:"msg_prefix"`
	ForwardMetaPrefix string `toml:"forward_meta_prefix"`
	OutboxPrefix      string `toml:"outbox_prefix"`
}

func (c *Config) PrivateAccountDomain() string {
	parts := strings.Split(c.PrivateAccountAddress, "@")
	return parts[len(parts)-1]
}

func (c *Config) PrivateAccountMailbox() string {
	parts := strings.Split(c.PrivateAccountAddress, "@")
	return parts[0]
}

func loadCloudConfig(lgr log15.Logger) *Config {
	bucketName := os.Getenv("S3_CONFIG_BUCKET")
	confPath := os.Getenv("S3_CONFIG_PATH")

	if bucketName == "" || confPath == "" {
		lgr.Error("no_s3_env_config_found")
		return nil
	}

	sess := session.Must(session.NewSession())
	s3client := s3.New(sess, &aws.Config{
		Region: aws.String("us-east-1"),
	})

	confResp, err := s3client.GetObject(&s3.GetObjectInput{
		Bucket: aws.String(bucketName),
		Key:    aws.String(confPath),
	})
	if err != nil {
		lgr.Error("fetch_lambda_conf_err", "err", err)
		return nil
	}

	defer confResp.Body.Close()

	var conf Config
	_, err = toml.DecodeReader(confResp.Body, &conf)
	if err != nil {
		lgr.Error("toml_config_decode_err", "err", err)
		return nil
	}

	return &conf
}

func loadConfig() *Config {
	lgr := log15.New()
	conf := loadCloudConfig(lgr)
	if conf != nil {
		lgr.Info("loaded_config_from_cloud")
		return conf
	}

	return loadLocalConfig()
}

func loadLocalConfig() *Config {
	tml, err := ioutil.ReadFile("config.toml")
	if err != nil {
		panic(err)
	}
	var conf Config
	err = toml.Unmarshal(tml, &conf)
	if err != nil {
		panic(err)
	}

	err = conf.validate()
	if err != nil {
		panic(err)
	}

	return &conf
}

func (c *Config) validate() error {
	if c.Domain == "" {
		return errors.New("domain must be set")
	}
	if c.PrivateAccountAddress == "" {
		return errors.New("private_address must be set")
	}

	if c.OutboundAddress == "" {
		return errors.New("outbound_address must be set")
	}

	if c.SSMPrefix == "" {
		return errors.New("ssm_prefix must be set")
	}
	if c.AwsRegion == "" {
		return errors.New("aws_region must be set")
	}

	if c.Bucket.Name == "" {
		return errors.New("bucket.name must be set")
	}
	if c.Bucket.MsgPrefix == "" {
		return errors.New("bucket.msg_prefix must be set")
	}
	if c.Bucket.ForwardMetaPrefix == "" {
		return errors.New("bucket.forward_meta_prefix must be set")
	}
	if c.Bucket.OutboxPrefix == "" {
		return errors.New("bucket.outbox_prefix must be set")
	}

	return nil
}
