BIN1=lambda-email
FUNCTION_NAME=my-lambda-email-function
BUCKET=my-lambda-src-bucket
VERSION=v1.0.0

$(BIN1): $(wildcard *.go) $(wildcard **/*.go) go.mod go.sum
	go test .
	go build -o $@ .

$(BIN1).zip: $(BIN) $(wildcard ./config.toml)
	rm -f $@
	zip -r $@ $^

.PHONY: upload
upload: $(BIN1).zip
	aws lambda update-function-code --function-name $(FUNCTION_NAME) --zip-file fileb://$(BIN1).zip
	rm $(BIN1)
	rm $(BIN1).zip

.PHONY: upload_s3
upload_s3: $(BIN1).zip
	aws s3 cp $(BIN1).zip "s3://$(BUCKET)/$(FUNCTION_NAME)/$(VERSION)/$(FUNCTION_NAME).zip"
