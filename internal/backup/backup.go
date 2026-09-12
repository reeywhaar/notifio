// Package backup copies config.json and data.json to a backup agent.
package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// maxReply is how much of a rejection is read back: enough for a sentence from the other end,
// not enough for a page of HTML to reach the log.
const maxReply = 2 << 10

// Name is what the archive is called in the request.
//
// backio-agent reads it for its extension only and assigns its own timestamped filename, so
// there is nothing here for a timestamp to do.
const Name = "notifio.tgz"

// Archive is a gzipped tar of the two files, with a digest of their contents.
type Archive struct {
	Body   []byte
	Digest string
}

// Build reads both files and renders the archive. A file that is not there is skipped: a fresh
// volume has no data.json yet.
func Build(paths []string, now time.Time) (*Archive, error) {
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	sum := sha256.New()

	for _, p := range paths {
		data, err := os.ReadFile(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		name := filepath.Base(p)
		fmt.Fprintf(sum, "%s:%d:", name, len(data))
		sum.Write(data)

		// 0600 throughout: the archive carries every credential notifio has, so an extracted
		// copy should not be readable by anyone else.
		hdr := &tar.Header{
			Typeflag: tar.TypeReg,
			Name:     name,
			Mode:     0o600,
			Size:     int64(len(data)),
			ModTime:  now.UTC().Truncate(time.Second),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if _, err := tw.Write(data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return &Archive{Body: out.Bytes(), Digest: hex.EncodeToString(sum.Sum(nil))}, nil
}

// Push posts one archive to a backio-agent.
//
// Two fields and no credential: the agent is reachable only from this project's own network,
// and it holds the token, the provider and the subdirectory. A compromised notifio cannot read,
// overwrite or delete a single existing backup, which is the entire point of the sidecar.
func Push(ctx context.Context, client *http.Client, url, name string, body []byte) error {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("backup", name)
	if err != nil {
		return err
	}
	if _, err := part.Write(body); err != nil {
		return err
	}
	if err := w.WriteField("name", name); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())

	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		// What it said, not just that it said no: the body underneath is where the rejected
		// token and the unreachable remote are named.
		said, _ := io.ReadAll(io.LimitReader(res.Body, maxReply))
		return fmt.Errorf("the backup agent answered %s: %s", res.Status, strings.TrimSpace(string(said)))
	}
	return nil
}
