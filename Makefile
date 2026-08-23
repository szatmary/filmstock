# filmstock — dumps in, media database out.
#
# Every target is resumable and none of them are incremental: the parse is a
# linear stream over the whole 26.5 GB dump, so a rebuild reads everything.

BIN     := ./filmstock
DUMPS   := dump
OUT     := out
WD      := wikidata.db
WORKERS := 18

ENWIKI  := https://dumps.wikimedia.org/enwiki/latest
WIKIDATA:= https://dumps.wikimedia.org/wikidatawiki/entities

DUMP    := $(DUMPS)/enwiki-latest-pages-articles-multistream.xml.bz2
INDEX   := $(DUMPS)/enwiki-latest-pages-articles-multistream-index.txt.bz2
PROPS   := $(DUMPS)/enwiki-latest-page_props.sql.gz
ENTITIES:= $(DUMPS)/latest-all.json.bz2

# aria2c saturates the link and resumes; wget is the fallback.
FETCH := $(shell command -v aria2c >/dev/null && echo "aria2c -c -x4 -s4 --dir=$(DUMPS) -o" || echo "wget -c -O $(DUMPS)/")
# lbzip2 decompresses in parallel; the 102 GB wikidata pass is bound on it.
BUNZIP := $(shell command -v lbzip2 >/dev/null && echo "lbzip2 -dc -n 20" || echo "bzip2 -dc")

.PHONY: build test dumps resolver extract index serve verify split join web dist clean-out help

help:
	@sed -n 's/^##//p' $(MAKEFILE_LIST)

## build      compile the binaries -> ./filmstock and ./filmstock-web
build:
	go build -o filmstock ./cmd/filmstock
	go build -o filmstock-web ./cmd/filmstock-web

## test       run the regression tests
test:
	go test ./...

## dumps      download the four source dumps (~129 GB)
dumps: $(DUMP) $(INDEX) $(PROPS) $(ENTITIES)

$(DUMP):
	mkdir -p $(DUMPS) && $(FETCH)$(notdir $@) $(ENWIKI)/$(notdir $@)
$(INDEX):
	mkdir -p $(DUMPS) && $(FETCH)$(notdir $@) $(ENWIKI)/$(notdir $@)
$(PROPS):
	mkdir -p $(DUMPS) && $(FETCH)$(notdir $@) $(ENWIKI)/$(notdir $@)
$(ENTITIES):
	mkdir -p $(DUMPS) && $(FETCH)$(notdir $@) $(WIKIDATA)/$(notdir $@)

## resolver   build wikidata.db: P179/P4908 edges, then page_id -> Q-id (~28 GB)
resolver: build $(ENTITIES) $(PROPS) $(INDEX)
	$(BUNZIP) $(ENTITIES) | $(BIN) build-wd-edges -db $(WD)
	$(BIN) build-qidmap -pageprops $(PROPS) -index $(INDEX) -db $(WD)
	@sqlite3 $(WD) \
	  "select 'wiki_qid          '||count(*) from wiki_qid" \
	  "select '  with page_id    '||count(*) from wiki_qid where page_id is not null" \
	  "select 'wd_part_of_series '||count(*) from wd_part_of_series" \
	  "select 'wd_episode_season '||count(*) from wd_episode_season"

## extract    dumps -> out/ records + out/search.db (one pass, ~48 min)
extract: build
	@test -s $(WD) || { echo "no $(WD) — run 'make resolver' first"; exit 1; }
	$(BIN) extract -dumps $(DUMPS) -out $(OUT) -cache $(WD) -workers $(WORKERS)
	@$(MAKE) --no-print-directory verify

## index      rebuild out/search.db from the records alone (no dump read)
index: build
	$(BIN) index -records $(OUT) -db $(OUT)/search.db -workers 16
	@$(MAKE) --no-print-directory verify

## verify     print what actually landed in the database
verify:
	@sqlite3 $(OUT)/search.db \
	  "select 'movies              '||count(*) from movies" \
	  "select 'events              '||count(*) from events" \
	  "select 'television_series   '||count(*) from television_series" \
	  "select 'television_episodes '||count(*) from television_episodes" \
	  "select 'people              '||count(*) from people" \
	  "select '  with qid          '||count(*) from people where qid is not null" \
	  "select 'credits             '||count(*) from credits"
	@echo "raw wikitext leaking into person names (must be 0):"
	@sqlite3 $(OUT)/search.db \
	  "select '  '||count(*) from people where name like '%[[%' or name like '%<br%'"
	@echo "episodes whose series_id has no series row (must be 0 — episode search"
	@echo "joins for the series title instead of storing a copy):"
	@sqlite3 $(OUT)/search.db \
	  "select '  '||count(*) from television_episodes e \
	   left join television_series t on t.id = e.series_id where t.id is null"

## serve      web UI on :8080
serve: build
	$(BIN) serve -db $(OUT)/search.db -movies $(OUT)/movies -television $(OUT)/television \
	  -events $(OUT)/events -addr :8080

## split      cut the index into git-committable parts (<100 MB each)
split: build
	$(BIN) split -db $(OUT)/search.db -out index

## join       reassemble the index from parts, verifying checksums
join: build
	$(BIN) join -in index -db $(OUT)/search.db

## web        the browser
web: build
	./filmstock-web -db $(OUT)/search.db -records $(OUT) -addr :8080

## dist       compress the database for distribution (zstd -19)
dist: $(OUT)/search.db
	zstd -19 -T0 -f -k $(OUT)/search.db -o $(OUT)/search.db.zst
	@printf "  %-22s %s\n" "search.db"     "$$(du -h --apparent-size $(OUT)/search.db     | cut -f1)"
	@printf "  %-22s %s\n" "search.db.zst" "$$(du -h --apparent-size $(OUT)/search.db.zst | cut -f1)"

## clean-out  delete the regenerated outputs; keeps dumps and the resolver cache
clean-out:
	rm -rf $(OUT)
