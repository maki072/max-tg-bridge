package bridge

import (
	"bytes"
	"fmt"
	"image/jpeg"
	"io"
	"net/http"
	"time"

	"golang.org/x/image/webp"
)

// httpDownload fetches a public URL into memory (bounded by maxBytes). MAX media
// URLs (i.oneme.ru / file-ms.oneme.ru) are directly fetchable without auth.
func httpDownload(url string, maxBytes int64) ([]byte, string, error) {
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("http %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes))
	if err != nil {
		return nil, "", err
	}
	return data, resp.Header.Get("Content-Type"), nil
}

// webpToJPEG converts a WebP image to JPEG (Telegram sendPhoto needs JPEG/PNG,
// MAX serves photos as WebP).
func webpToJPEG(data []byte) ([]byte, error) {
	img, err := webp.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: 88}); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
