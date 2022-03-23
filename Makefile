BIN=lambda-email
FUNCTION_NAME=my-lambda-email-function
BUCKET=my-lambda-src-bucket
VERSION=v1.0.0

$(BIN): $(wildcard *.go) $(wildcard **/*.go) go.mod go.sum
	go test .
	go build -o $@ .

$(BIN).zip: $(BIN) $(wildcard ./config.toml)
	rm -f $@
	zip -r $@ $^

.PHONY: upload
upload: $(BIN).zip
	aws lambda update-function-code --function-name $(FUNCTION_NAME) --zip-file fileb://$(BIN).zip
	rm $(BIN)
	rm $(BIN).zip

.PHONY: upload_s3
upload_s3: $(BIN).zip
	aws s3 cp $(BIN).zip "s3://$(BUCKET)/$(FUNCTION_NAME)/$(VERSION)/$(FUNCTION_NAME).zip"
