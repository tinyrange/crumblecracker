package download

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

type LimitError struct {
	Limit  int64
	Actual int64
}

func (e *LimitError) Error() string {
	return fmt.Sprintf("download exceeds byte budget: limit=%d actual=%d", e.Limit, e.Actual)
}

type LengthError struct {
	Expected int64
	Actual   int64
}

func (e *LengthError) Error() string {
	return fmt.Sprintf("download length mismatch: expected=%d actual=%d", e.Expected, e.Actual)
}

type DigestError struct {
	Expected string
	Actual   string
}

func (e *DigestError) Error() string {
	return fmt.Sprintf("download digest mismatch: expected=%s actual=%s", e.Expected, e.Actual)
}

type Budget struct {
	MaxBytes       int64
	ExpectedBytes  int64
	ExpectedSHA256 string
}

type LimitWriter struct {
	dst     io.Writer
	limit   int64
	written int64
}

func NewLimitWriter(dst io.Writer, maxBytes int64) (*LimitWriter, error) {
	if dst == nil || maxBytes <= 0 {
		return nil, fmt.Errorf("positive output byte budget and writer are required")
	}
	return &LimitWriter{dst: dst, limit: maxBytes}, nil
}

func (w *LimitWriter) Write(p []byte) (int, error) {
	remaining := w.limit - w.written
	if remaining <= 0 {
		return 0, &LimitError{Limit: w.limit, Actual: w.written + int64(len(p))}
	}
	requested := len(p)
	overBudget := int64(requested) > remaining
	if overBudget {
		p = p[:remaining]
	}
	n, err := w.dst.Write(p)
	w.written += int64(n)
	if err != nil {
		return n, err
	}
	if overBudget {
		return n, &LimitError{Limit: w.limit, Actual: w.written + int64(requested-n)}
	}
	if n < requested {
		return n, io.ErrShortWrite
	}
	return n, nil
}

func ReadAll(ctx context.Context, resp *http.Response, budget Budget) ([]byte, error) {
	var out bytesWriter
	if _, err := Copy(ctx, &out, resp, budget); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func ReadAllReader(ctx context.Context, r io.Reader, maxBytes int64) ([]byte, error) {
	if ctx == nil {
		return nil, fmt.Errorf("download context is required")
	}
	if maxBytes <= 0 {
		return nil, fmt.Errorf("positive expanded byte budget is required")
	}
	var out bytesWriter
	n, err := io.Copy(&out, &contextReader{ctx: ctx, r: io.LimitReader(r, maxBytes+1)})
	if err != nil {
		return nil, err
	}
	if n > maxBytes {
		return nil, &LimitError{Limit: maxBytes, Actual: n}
	}
	return out.Bytes(), nil
}

func CopyReader(ctx context.Context, dst io.Writer, r io.Reader, maxBytes int64) (int64, error) {
	if ctx == nil {
		return 0, fmt.Errorf("download context is required")
	}
	if maxBytes <= 0 {
		return 0, fmt.Errorf("positive expanded byte budget is required")
	}
	n, err := io.Copy(dst, &contextReader{ctx: ctx, r: io.LimitReader(r, maxBytes+1)})
	if err != nil {
		return n, err
	}
	if n > maxBytes {
		return n, &LimitError{Limit: maxBytes, Actual: n}
	}
	return n, nil
}

// BoundResponse applies a wire-byte budget to callers that consume the body
// incrementally. A zero max uses the server-declared Content-Length as the
// exact budget; chunked/unsized artifact responses are rejected because they
// cannot be admitted safely before consumption.
func BoundResponse(resp *http.Response, maxBytes int64) error {
	if resp == nil || resp.Body == nil {
		return fmt.Errorf("download response body is required")
	}
	if resp.ContentLength <= 0 {
		return fmt.Errorf("artifact response requires a positive content length")
	}
	if maxBytes > 0 && resp.ContentLength > maxBytes {
		return &LimitError{Limit: maxBytes, Actual: resp.ContentLength}
	}
	resp.Body = &boundedBody{body: resp.Body, expected: resp.ContentLength, remaining: resp.ContentLength}
	return nil
}

type boundedBody struct {
	body      io.ReadCloser
	expected  int64
	remaining int64
	done      bool
}

func (r *boundedBody) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	if r.remaining == 0 {
		var probe [1]byte
		n, err := r.body.Read(probe[:])
		if n > 0 {
			return 0, &LengthError{Expected: r.expected, Actual: r.expected + int64(n)}
		}
		if err == nil {
			return 0, &LengthError{Expected: r.expected, Actual: r.expected + 1}
		}
		r.done = true
		return 0, io.EOF
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.body.Read(p)
	r.remaining -= int64(n)
	if errors.Is(err, io.EOF) && r.remaining != 0 {
		r.done = true
		return n, &LengthError{Expected: r.expected, Actual: r.expected - r.remaining}
	}
	return n, err
}

func (r *boundedBody) Close() error { return r.body.Close() }

func Copy(ctx context.Context, dst io.Writer, resp *http.Response, budget Budget) (int64, error) {
	if ctx == nil {
		return 0, fmt.Errorf("download context is required")
	}
	if resp == nil || resp.Body == nil {
		return 0, fmt.Errorf("download response body is required")
	}
	if budget.MaxBytes <= 0 {
		return 0, fmt.Errorf("positive download byte budget is required")
	}
	if resp.ContentLength > budget.MaxBytes {
		return 0, &LimitError{Limit: budget.MaxBytes, Actual: resp.ContentLength}
	}
	if budget.ExpectedBytes > 0 && resp.ContentLength >= 0 && resp.ContentLength != budget.ExpectedBytes {
		return 0, &LengthError{Expected: budget.ExpectedBytes, Actual: resp.ContentLength}
	}

	var digest hash.Hash
	w := dst
	if budget.ExpectedSHA256 != "" {
		digest = sha256.New()
		w = io.MultiWriter(dst, digest)
	}
	reader := &contextReader{ctx: ctx, r: io.LimitReader(resp.Body, budget.MaxBytes+1)}
	n, err := io.Copy(w, reader)
	if err != nil {
		return n, err
	}
	if n > budget.MaxBytes {
		return n, &LimitError{Limit: budget.MaxBytes, Actual: n}
	}
	if budget.ExpectedBytes > 0 && n != budget.ExpectedBytes {
		return n, &LengthError{Expected: budget.ExpectedBytes, Actual: n}
	}
	if resp.ContentLength >= 0 && n != resp.ContentLength {
		return n, &LengthError{Expected: resp.ContentLength, Actual: n}
	}
	if digest != nil {
		actual := hex.EncodeToString(digest.Sum(nil))
		expected := trimSHA256(budget.ExpectedSHA256)
		if actual != expected {
			return n, &DigestError{Expected: "sha256:" + expected, Actual: "sha256:" + actual}
		}
	}
	return n, nil
}

func trimSHA256(value string) string {
	if len(value) > 7 && value[:7] == "sha256:" {
		return value[7:]
	}
	return value
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.r.Read(p)
	if err != nil && r.ctx.Err() != nil {
		return n, r.ctx.Err()
	}
	return n, err
}

type bytesWriter struct{ data []byte }

func (w *bytesWriter) Write(p []byte) (int, error) {
	w.data = append(w.data, p...)
	return len(p), nil
}

func (w *bytesWriter) Bytes() []byte { return w.data }

func FilesystemBudget(path string) (int64, error) {
	probe := path
	for {
		if info, err := os.Stat(probe); err == nil {
			if !info.IsDir() {
				probe = filepath.Dir(probe)
			}
			break
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			break
		}
		probe = parent
	}
	available, err := filesystemAvailableBytes(probe)
	if err != nil {
		return 0, err
	}
	if available == 0 || available > uint64(^uint64(0)>>1) {
		return 0, fmt.Errorf("filesystem at %s has no representable available-byte budget", probe)
	}
	return int64(available), nil
}
