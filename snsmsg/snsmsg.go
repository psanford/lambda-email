package snsmsg

type Msg struct {
	ID           string   `json:"id"`
	From         []string `json:"from"`
	To           []string `json:"to"`
	Subject      string   `json:"subject"`
	Date         string   `json:"date"`
	PresignedURL string   `json:"presigned_url"`
}
