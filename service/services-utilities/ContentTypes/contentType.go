package ContentTypes

import (
	"bytes"
	"encoding/json"
	"io"
	"io/ioutil"
	"net/http"
	"strings"
)

// ContentType is an interface which different types of content-type implement it
type ContentType interface {
	GetFileFromRequest() *bytes.Buffer
	BodyAfterScanning([]byte) string
}

// GetContentType is used for getting the content-type in the request and returning an instance from the suitable struct
func GetContentType(req *http.Request) ContentType {
	contentType := req.Header.Get("Content-Type")

	// if the content-type string has "multipart/", the func will return a new instance from MultipartForm struct
	if strings.HasPrefix(contentType, "multipart/") {
		return NewMultipartForm(ParsingRequest(req))
	} else if strings.HasPrefix(contentType, "application/json") && !strings.Contains(contentType, "+") {
		// Only treat plain application/json as JSON. application/json+protobuf and similar
		// are binary formats and must be passed through unchanged to avoid corrupting the body.
		//in this case there are two odds
		//first is that file encoded in base64 and second is that file is actually a JSON file,
		//so we convert the JSON file to a map
		var data map[string]interface{}
		body, _ := ioutil.ReadAll(req.Body)

		// If the JSON is gzip compressed, we cannot unmarshal it to check "Base64".
		// We must pass it as a regular file untouched.
		if req.Header.Get("Content-Encoding") != "" {
			return NewRegularFile(bytes.NewBuffer(body), false)
		}

		err := json.Unmarshal(body, &data)
		if err != nil {
			// If it fails to unmarshal (e.g. malformed JSON or other encodings), pass it untouched
			return NewRegularFile(bytes.NewBuffer(body), false)
		}

		//checking if there is key equals "Base64", the file would be encoded
		// if no, the file is a norma JSON file
		_, found := data["Base64"]
		if found {
			return NewEncodedFile(data)
		} else {
			// We already have the original unadulterated body buffer. Pass it untouched
			// rather than re-marshaling the map, which destroys key order & formatting.
			return NewRegularFile(bytes.NewBuffer(body), false)
		}
	}

	//if this code be reached, the file will be a normal file
	buf := &bytes.Buffer{}
	io.Copy(buf, req.Body)
	return NewRegularFile(buf, false)
}
