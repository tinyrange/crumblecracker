package download

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFilesystemBudgetAcceptsFilePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "layer.tar")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	budget, err := FilesystemBudget(path)
	if err != nil {
		t.Fatal(err)
	}
	if budget <= 0 {
		t.Fatalf("budget = %d, want a positive number of available bytes", budget)
	}
}

func TestCopyRejectsOversizeBeforePublication(t *testing.T) {
	resp := &http.Response{Body: io.NopCloser(strings.NewReader("oversized")), ContentLength: 9}
	var dst strings.Builder
	_, err := Copy(context.Background(), &dst, resp, Budget{MaxBytes: 8})
	var limitErr *LimitError
	if !errors.As(err, &limitErr) || limitErr.Limit != 8 || limitErr.Actual != 9 {
		t.Fatalf("error = %#v", err)
	}
	if dst.Len() != 0 {
		t.Fatalf("published %d bytes before rejecting content length", dst.Len())
	}
}

func TestCopyReportsLengthAndDigestMismatch(t *testing.T) {
	for _, tc := range []struct {
		name   string
		budget Budget
		match  any
	}{
		{name: "length", budget: Budget{MaxBytes: 8, ExpectedBytes: 4}, match: &LengthError{}},
		{name: "digest", budget: Budget{MaxBytes: 8, ExpectedSHA256: strings.Repeat("0", 64)}, match: &DigestError{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{Body: io.NopCloser(strings.NewReader("abc")), ContentLength: 3}
			_, err := Copy(context.Background(), io.Discard, resp, tc.budget)
			switch target := tc.match.(type) {
			case *LengthError:
				if !errors.As(err, &target) {
					t.Fatalf("error = %v", err)
				}
			case *DigestError:
				if !errors.As(err, &target) {
					t.Fatalf("error = %v", err)
				}
			}
		})
	}
}

func TestBoundResponseReportsTruncatedBody(t *testing.T) {
	resp := &http.Response{Body: io.NopCloser(strings.NewReader("abc")), ContentLength: 4}
	if err := BoundResponse(resp, 8); err != nil {
		t.Fatal(err)
	}
	_, err := io.ReadAll(resp.Body)
	var lengthErr *LengthError
	if !errors.As(err, &lengthErr) || lengthErr.Expected != 4 || lengthErr.Actual != 3 {
		t.Fatalf("error = %#v", err)
	}
}

func TestReadAllReaderHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := ReadAllReader(ctx, strings.NewReader("artifact"), 16)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context cancellation", err)
	}
}
