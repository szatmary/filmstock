package dump

import (
	"bufio"
	"encoding/xml"
	"fmt"
	"io"
	"os"
)

// RunStream decodes every <page> from a plain MediaWiki XML stream.
//
// This is the incremental (adds-changes) path. Those dumps are a single bz2
// stream with no multistream offset index, so none of the seek-and-parallelise
// machinery applies — there is one stream, read start to finish.
//
// It is also far smaller work than the full dump: about 4.5 GB decompressed per
// day against roughly 90 GB, because it carries only pages that changed.
//
// A page in an adds-changes dump carries EVERY revision made in the window
// rather than just the current one — about two per page. Page.Text is bound to a
// scalar, so decoding keeps the last, and revisions are in chronological order,
// so the last is the newest. See TestMultipleRevisionsKeepTheNewest.
func RunStream(r io.Reader, handle func(Page)) error {
	dec := xml.NewDecoder(bufio.NewReaderSize(r, 1<<20))
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("dump: reading stream: %w", err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "page" {
			continue
		}
		var p Page
		if err := dec.DecodeElement(&p, &se); err != nil {
			return fmt.Errorf("dump: decoding page: %w", err)
		}
		handle(p)
	}
}

// DumpSize is the compressed size of a dump, the denominator for progress.
func DumpSize(path string) (int64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}
