package session

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
)

// Encode serializes the envelope, gzipping when it exceeds GzipThreshold.
// The returned filename is AttachmentName or AttachmentNameGz accordingly.
func Encode(env *Envelope) (filename string, data []byte, err error) {
	raw, err := json.Marshal(env)
	if err != nil {
		return "", nil, err
	}
	if len(raw) <= GzipThreshold {
		return AttachmentName, raw, nil
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		return "", nil, err
	}
	if err := zw.Close(); err != nil {
		return "", nil, err
	}
	return AttachmentNameGz, buf.Bytes(), nil
}
