# lambda-email

lambda-email is an AWS SES email forwarding and routing service. Its primarily designed for forwarding all email for a domain to a single mailbox hosted on a normal email service (gmail, outlook, etc). It has the following additional features:

- Forward all email from a domain to a single private email address
- Replies from that email address automatically reply to the original sender
- Custom routing rules to SNS topics allow for additional programmatic processing of messages
- Outbound emails are also possible (not the primary use case)

I use lambda-email for a number of different use cases:

- Every service I signup for gets a unique email address: `service-a@proxy.example.com`. This allows for easy tracking of third party services that share your email address with other companies
- For daily news letters, I will forward those emails to another lambda function that converts the email to an epub and pushes it to my reMarkable2 e-ink reader
- I have a number of special addresses that when they receive emails from my private address will invoke a lambda function to do things like:
  - Convert the email text to speech using AWS Polly
  - Fetch and save an archive of a URL
  - Convert the email to an SMS message and forward that to a phone number
  - Parse order confirmations and record the receipt to a database


## Setup

### S3 bucket

You'll need an s3 bucket for storing incoming messages and metadata.

It is recommended that you use the following prefixes in this bucket:

    # msg_prefix is where incoming messages are stored
    msg_prefix          = "/email"
    # forward_meta_prefix is where lambda-email stores meta data required for
    # replying to messages properly
    forward_meta_prefix = "/forward-metadata"
    # outbox_prefix is where pending outbound email are stored
    outbox_prefix       = "/outbox"

SES should be configured to write messages to this bucket in the `msg_prefix` path.

The lambda function needs read access to `msg_prefix` and read/write access for `forward_meta_prefix`.

### Lambda setup

Create a Go lambda function type. Give it access to the s3 bucket. You created above. Make it triggered on SES incoming messages. Give it SES send permission.

### SES Setup

You need a verified SES domain. You'll also want the ability to send from any address on this domain.

Create a SES receipt rule that saves inbound emails to an s3 bucket. Use "email" as the object key prefix.
Create another receipt rule that invokes this lambda function.

### Lambda function configuration

The lambda function's configuration primarily comes from a toml config file. This file can either be included in the function zip, or it can be stored in an external s3 bucket that is readable by the lambda function (specified by environment variables).

Including the config file directly in the zip is the simplest way to use lambda-email. Simply create a file call `config.toml` in the root project directory. Running `make lambda-email.zip` will build the lambda function code and create a zip that includes the binary and your config file. There is an example config file called `config.example.toml` that you can use as a template.


## Configuration and message routing

By default, all messages to your domain will be forwarded to your private email address. If you send an email from your private email address to your domain, it will be assumed to be a reply to an existing message and won't be forwarded.

You can create rules to forward specific messages to an SNS topic. This makes it easy to invoke other lambda functions to get additional programmatic behavior for certain events.

Currently routing rules only match on the from and to fields of the email. In the future this could be extended to match on additional fields.

Example:
```
[[route]]
# When we get an email from private@gmail.example.com addressed to
# sms@proxyemail.example.com, invoke the email_to_sms sns topic
src = "__PRIVATE_ADDRESS__"
dst = "sms@proxyemail.example.com"
sns = "arn:aws:sns:us-east-1:123456789012:email_to_sms"
forward = false

[[route]]
# when we get any email addressed to mailinglist@proxyemail.example.com
# ivoke the process_mailinglist sns topic and also forward it to
# the private address
src = "/.*/"
dst = "moneystuff@proxyemail.example.com"
sns = "arn:aws:sns:us-east-1:123456789012:process_mailinglist"
forward = true

[[route]]
# send any emails addressed to sales@proxyemail.example.com to
# our blackhole sns topic. Don't forward them. Include
# suspect messages for forwarding.
src = "/.*/"
dst = "sales@proxyemail.example.com"
sns = "arn:aws:sns:us-east-1:123456789012:email_blackhole"
forward = false
allow_suspect_messages = true
```

## A warning about bounced emails to your private address

If someone sends you spam, or a virus, it is possible that that message will get bounced by your private address email service. If that occurs we will log and generate a lambda execution error. In order to avoid having sending reputation issues, you will want to monitor for these types of failures and handle them. Dealing with lambda execution errors is outside the scope of this document, but I recommend at least setting up a cloudwatch alert for function execution errors.

## Outbound emails

`lambda-email` is primarily meant for receiving inbound emails and occasionally replying to them. However it is sometimes useful to be able to send outbound emails that are not in reply to an existing email.

This is possible to do with lambda-email, but its quite clunky right now.

Compose and send and email from your private email address to the configured `outbound_address` (normally `outbound@proxy.example.com`). This will drop that email into the `outbox` prefix of your s3 bucket. You can then use the `lambda-email-outbox` cli tool to list the pending messages in your `outbox` and to send one to a specific address from a proxy address.

At some point I might streamline this process so you don't have to use a cli tool.
