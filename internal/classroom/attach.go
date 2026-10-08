package classroom

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	gclassroom "google.golang.org/api/classroom/v1"
	"google.golang.org/api/drive/v3"

	"schoolwork-check/internal/extract"
	"schoolwork-check/internal/filecache"
	"schoolwork-check/internal/model"
)

// Google-native Drive types and the text format each exports to.
const (
	mimeGoogleAppsPrefix = "application/vnd.google-apps."
	mimeGoogleDoc        = "application/vnd.google-apps.document"
	mimeGoogleSheet      = "application/vnd.google-apps.spreadsheet"
	mimeGoogleSlides     = "application/vnd.google-apps.presentation"
	mimeGoogleForm       = "application/vnd.google-apps.form"
	mimeLink             = "text/uri-list"
	mimeYouTube          = "video/youtube"
)

var exportFormats = map[string]string{
	mimeGoogleDoc:    "text/plain",
	mimeGoogleSheet:  "text/csv",
	mimeGoogleSlides: "text/plain",
}

// material is the union of the four attachment shapes Classroom uses. Both
// CourseWork.Materials and StudentSubmission attachments collapse into it so
// teacher materials and student work share one conversion path.
type material struct {
	drive   *gclassroom.DriveFile
	link    *gclassroom.Link
	youtube *gclassroom.YouTubeVideo
	form    *gclassroom.Form
}

func fromMaterials(ms []*gclassroom.Material) []material {
	out := make([]material, 0, len(ms))
	for _, m := range ms {
		if m == nil {
			continue
		}
		mat := material{link: m.Link, youtube: m.YoutubeVideo, form: m.Form}
		if m.DriveFile != nil {
			mat.drive = m.DriveFile.DriveFile
		}
		out = append(out, mat)
	}
	return out
}

func fromAttachments(as []*gclassroom.Attachment) []material {
	out := make([]material, 0, len(as))
	for _, a := range as {
		if a == nil {
			continue
		}
		out = append(out, material{drive: a.DriveFile, link: a.Link, youtube: a.YouTubeVideo, form: a.Form})
	}
	return out
}

// attachments converts materials to model.Attachment, extracting text where
// possible. Failures become ContentError; they never fail the task.
func (c *Client) attachments(ctx context.Context, mats []material, st *stats) []model.Attachment {
	if len(mats) == 0 {
		return nil
	}
	out := make([]model.Attachment, 0, len(mats))
	for _, m := range mats {
		switch {
		case m.drive != nil:
			out = append(out, c.driveAttachment(ctx, m.drive, st))
		case m.link != nil:
			name := m.link.Title
			if name == "" {
				name = m.link.Url
			}
			out = append(out, model.Attachment{Name: name, URL: m.link.Url, MimeType: mimeLink})
		case m.youtube != nil:
			out = append(out, model.Attachment{Name: m.youtube.Title, URL: m.youtube.AlternateLink, MimeType: mimeYouTube})
		case m.form != nil:
			url := m.form.FormUrl
			if url == "" {
				url = m.form.ResponseUrl
			}
			out = append(out, model.Attachment{Name: m.form.Title, URL: url, MimeType: mimeGoogleForm})
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// driveAttachment resolves a Drive file's metadata and, when extraction is
// enabled, its text. Google-native docs are exported; uploaded files are
// downloaded and handed to the extract package.
func (c *Client) driveAttachment(ctx context.Context, df *gclassroom.DriveFile, st *stats) model.Attachment {
	a := model.Attachment{Name: df.Title, URL: df.AlternateLink}
	if !c.opts.ExtractAttachments {
		return a
	}
	if c.drv == nil {
		a.ContentError = "drive client unavailable"
		return a
	}

	var meta *drive.File
	err := c.retry(ctx, "drive.files.get", func() error {
		var e error
		meta, e = c.drv.Files.Get(df.Id).
			Fields("id,name,mimeType,size,version,modifiedTime,md5Checksum").
			SupportsAllDrives(true).
			Context(ctx).Do()
		return e
	})
	if err != nil {
		st.failures.Add(1)
		a.ContentError = "drive metadata: " + errText(err)
		return a
	}
	if a.Name == "" {
		a.Name = meta.Name
	}
	a.MimeType = meta.MimeType
	a.SizeBytes = meta.Size

	key, version := "drive:"+df.Id, c.driveVersion(meta)
	if e, ok := c.opts.FileCache.Get(key, version); ok {
		a.Content, a.ContentError, a.Truncated = e.Content, e.ContentError, e.Truncated
		return a
	}

	var permanent bool
	switch {
	case exportFormats[meta.MimeType] != "":
		permanent = c.exportDriveFile(ctx, df.Id, exportFormats[meta.MimeType], &a, st)
	case strings.HasPrefix(meta.MimeType, mimeGoogleAppsPrefix):
		a.ContentError = "google-native type not exportable to text"
	default:
		permanent = c.downloadDriveFile(ctx, df.Id, meta, &a, st)
	}
	if permanent {
		c.opts.FileCache.Put(key, version, filecache.Entry{Content: a.Content, ContentError: a.ContentError, Truncated: a.Truncated})
	}
	return a
}

// driveVersion is what a cached extraction of this file is valid for. Drive
// bumps version on every change, Google Docs included; modifiedTime and the
// checksum cover files where version is absent. The extraction limits change
// the result, so they are part of it. No version information means "".
func (c *Client) driveVersion(meta *drive.File) string {
	if meta.Version == 0 && meta.ModifiedTime == "" && meta.Md5Checksum == "" {
		return ""
	}
	return fmt.Sprintf("%d|%s|%s|%d|%d", meta.Version, meta.ModifiedTime, meta.Md5Checksum,
		c.limit(), c.opts.MaxAttachmentBytes)
}

// exportDriveFile exports a Google-native file as text. It reports whether
// the outcome is worth caching (only success is).
func (c *Client) exportDriveFile(ctx context.Context, fileID, exportMime string, a *model.Attachment, st *stats) bool {
	var body string
	var truncated bool
	err := c.retry(ctx, "drive.files.export", func() error {
		res, e := c.drv.Files.Export(fileID, exportMime).Context(ctx).Download()
		if e != nil {
			return e
		}
		defer res.Body.Close()
		body, truncated, e = readCapped(res.Body, c.limit())
		return e
	})
	if err != nil {
		st.failures.Add(1)
		a.ContentError = "drive export: " + errText(err)
		return false
	}
	a.Content = body
	a.Truncated = truncated
	st.extracted.Add(1)
	return true
}

// downloadDriveFile downloads an uploaded file and extracts its text. It
// reports whether the outcome is worth caching: text, or an unsupported type.
func (c *Client) downloadDriveFile(ctx context.Context, fileID string, meta *drive.File, a *model.Attachment, st *stats) bool {
	if c.opts.MaxAttachmentBytes > 0 && meta.Size > c.opts.MaxAttachmentBytes {
		a.ContentError = fmt.Sprintf("file too large: %d bytes > limit %d", meta.Size, c.opts.MaxAttachmentBytes)
		return false
	}
	// extract.Text is a package-level var filled in by the extraction
	// package; guard against it being unset.
	textFn := extract.Text
	if textFn == nil {
		a.ContentError = "extractor unavailable"
		return false
	}

	var res extract.Result
	err := c.retry(ctx, "drive.files.download", func() error {
		resp, e := c.drv.Files.Get(fileID).SupportsAllDrives(true).Context(ctx).Download()
		if e != nil {
			return e
		}
		defer resp.Body.Close()
		var body io.Reader = resp.Body
		if c.opts.MaxAttachmentBytes > 0 {
			body = io.LimitReader(body, c.opts.MaxAttachmentBytes)
		}
		res, e = textFn(ctx, extract.Input{
			Name:     a.Name,
			MimeType: meta.MimeType,
			Reader:   body,
			Limit:    c.limit(),
		})
		return e
	})
	if err != nil {
		st.failures.Add(1)
		a.ContentError = errText(err)
		var unsup *extract.UnsupportedError
		return errors.As(err, &unsup)
	}
	a.Content = res.Text
	a.Truncated = res.Truncated
	st.extracted.Add(1)
	return true
}

// limit is the per-attachment cap on extracted text.
func (c *Client) limit() int {
	if c.opts.MaxExtractedText > 0 {
		return c.opts.MaxExtractedText
	}
	return extract.DefaultLimit
}

// readCapped reads at most limit bytes, reporting whether more were available.
func readCapped(r io.Reader, limit int) (string, bool, error) {
	if limit <= 0 {
		limit = extract.DefaultLimit
	}
	b, err := io.ReadAll(io.LimitReader(r, int64(limit)+1))
	if err != nil {
		return "", false, err
	}
	if len(b) > limit {
		return string(b[:limit]), true, nil
	}
	return string(b), false, nil
}

// errText renders an error for an Attachment.ContentError field: short,
// single-line, no stack of wrapping noise.
func errText(err error) string {
	s := strings.TrimSpace(err.Error())
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 400 {
		s = s[:400] + "..."
	}
	return s
}
