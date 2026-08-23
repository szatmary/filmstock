package filmstock

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The record hierarchy is the repository. Everything else — index.db, derived
// indexes — is derived and can be deleted and rebuilt from these files without
// touching a dump.
//
//	out/movies/<shard>/<page_id>.json.gz
//	out/television/<shard>/<page_id>.json.gz
//	out/people/<shard>/<qid>.json.gz
//	out/text/<shard>/<page_id>.txt.gz     full-text corpus
//	out/manifest.jsonl                    kind, id, content hash
//
// A record's path is a pure function of its identity, never its title. Sharding
// on md5(title) meant a renamed article landed in a different file and the old
// one lingered forever as an orphan; ingest is additive, so nothing ever cleaned
// it up. Keying on page_id makes a re-extract overwrite in place.
const (
	KindMovie      = "movies"
	KindTelevision = "television"
	KindPerson     = "people"
	KindText       = "text"
	KindEvent      = "events" // award ceremonies and film festivals

	shardCount = 256
)

// RecordPath returns the path for one record, relative to the output root.
// The shard is derived from the id so that identity alone determines location.
//
// The shard is taken from |id|: ids are negative for people identified only by a
// link target rather than a Q-id, and Go's % yields a negative remainder, which
// produced directories named "-1f" and 489 shards instead of 256. The sign stays
// in the filename, where it still separates the two identity spaces.
func RecordPath(root, kind string, id int64, ext string) string {
	shard := id % shardCount
	if shard < 0 {
		shard = -shard
	}
	return filepath.Join(root, kind, fmt.Sprintf("%02x", shard),
		fmt.Sprintf("%d%s", id, ext))
}

// ReadRecordJSON decodes one record into v.
func ReadRecordJSON(path string, v any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	return json.NewDecoder(gz).Decode(v)
}

// WalkRecords calls fn for every record of a kind. This is how index reads the
// repository — it never needs to know how records were produced.
func WalkRecords(root, kind string, fn func(path string) error) error {
	dir := filepath.Join(root, kind)
	return filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, ".gz") || strings.HasSuffix(p, ".tmp") {
			return nil
		}
		return fn(p)
	})
}
