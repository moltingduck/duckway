package client

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const ccInboundAttachmentMaxBytes = discordPostFileMaxBytes

type downloadedCCAttachment struct {
	Filename    string
	Path        string
	ContentType string
	Size        int64
}

func (w *CCWatch) augmentPromptWithAttachments(ctx context.Context, handle string, msg payloadMessageCreate) (string, error) {
	files, err := w.downloadMessageAttachments(ctx, handle, msg.ID, msg.Attachments)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	content := strings.TrimSpace(msg.Content)
	if content != "" {
		b.WriteString(content)
		b.WriteString("\n\n")
	}
	b.WriteString("User uploaded file")
	if len(files) != 1 {
		b.WriteString("s")
	}
	b.WriteString(" to Discord. They have been downloaded on this client:\n")
	for _, file := range files {
		fmt.Fprintf(&b, "- %s", file.Path)
		if file.Filename != "" {
			fmt.Fprintf(&b, " (filename: %s", file.Filename)
			if file.ContentType != "" {
				fmt.Fprintf(&b, ", content_type: %s", file.ContentType)
			}
			if file.Size > 0 {
				fmt.Fprintf(&b, ", size: %d bytes", file.Size)
			}
			b.WriteString(")")
		}
		b.WriteByte('\n')
	}
	b.WriteString("\nUse the local file path above if you need to inspect the upload.")
	return b.String(), nil
}

func (w *CCWatch) downloadMessageAttachments(ctx context.Context, handle, messageID string, attachments []discordAttachmentRef) ([]downloadedCCAttachment, error) {
	if len(attachments) == 0 {
		return nil, nil
	}
	if strings.TrimSpace(messageID) == "" {
		messageID = "unknown-message"
	}
	dir := filepath.Join(w.configDir, "cc-attachments", safeAttachmentPathPart(handle), safeAttachmentPathPart(messageID))
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("mkdir attachment dir: %w", err)
	}
	client := &http.Client{Timeout: 60 * time.Second}
	out := make([]downloadedCCAttachment, 0, len(attachments))
	for i, att := range attachments {
		if att.Size > ccInboundAttachmentMaxBytes {
			return nil, fmt.Errorf("%s is too large: %d bytes exceeds 25 MiB", safeAttachmentDisplayName(att.Filename), att.Size)
		}
		source := strings.TrimSpace(att.URL)
		if source == "" {
			source = strings.TrimSpace(att.ProxyURL)
		}
		if err := validateDiscordAttachmentURL(source); err != nil {
			return nil, fmt.Errorf("%s has invalid url: %w", safeAttachmentDisplayName(att.Filename), err)
		}
		filename := uniqueAttachmentFilename(dir, i, safeAttachmentDisplayName(att.Filename))
		path := filepath.Join(dir, filename)
		size, err := downloadDiscordAttachment(ctx, client, source, path)
		if err != nil {
			return nil, fmt.Errorf("download %s: %w", filename, err)
		}
		out = append(out, downloadedCCAttachment{
			Filename:    filename,
			Path:        path,
			ContentType: strings.TrimSpace(att.ContentType),
			Size:        size,
		})
	}
	return out, nil
}

func validateDiscordAttachmentURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Host == "" {
		return fmt.Errorf("missing host")
	}
	if os.Getenv("DUCKWAY_CC_ALLOW_INSECURE_ATTACHMENT_URLS") == "1" {
		if u.Scheme != "https" && u.Scheme != "http" {
			return fmt.Errorf("unsupported scheme %q", u.Scheme)
		}
		return nil
	}
	if u.Scheme != "https" {
		return fmt.Errorf("unsupported scheme %q", u.Scheme)
	}
	switch strings.ToLower(u.Hostname()) {
	case "cdn.discordapp.com", "media.discordapp.net", "attachments.discordapp.net":
		return nil
	default:
		return fmt.Errorf("unsupported host %q", u.Hostname())
	}
}

func downloadDiscordAttachment(ctx context.Context, client *http.Client, source, dest string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, fmt.Errorf("unexpected HTTP status %s", resp.Status)
	}
	if resp.ContentLength > ccInboundAttachmentMaxBytes {
		return 0, fmt.Errorf("file too large: %d bytes exceeds 25 MiB", resp.ContentLength)
	}
	tmp := dest + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return 0, err
	}
	n, copyErr := io.Copy(f, io.LimitReader(resp.Body, ccInboundAttachmentMaxBytes+1))
	closeErr := f.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		return 0, copyErr
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return 0, closeErr
	}
	if n > ccInboundAttachmentMaxBytes {
		_ = os.Remove(tmp)
		return 0, fmt.Errorf("file too large: exceeds 25 MiB")
	}
	if n == 0 {
		_ = os.Remove(tmp)
		return 0, fmt.Errorf("file is empty")
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return 0, err
	}
	return n, nil
}

func safeAttachmentPathPart(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "unknown"
	}
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(raw) {
		ok := r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-'
		if ok {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "unknown"
	}
	if len(out) > 80 {
		out = out[:80]
	}
	return out
}

func uniqueAttachmentFilename(dir string, index int, name string) string {
	name = safeAttachmentDisplayName(name)
	if name == "attachment" {
		name = fmt.Sprintf("attachment-%d", index+1)
	}
	candidate := name
	for n := 2; ; n++ {
		if _, err := os.Stat(filepath.Join(dir, candidate)); os.IsNotExist(err) {
			return candidate
		}
		ext := filepath.Ext(name)
		base := strings.TrimSuffix(name, ext)
		candidate = fmt.Sprintf("%s-%d%s", base, n, ext)
	}
}
