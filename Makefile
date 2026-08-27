# filmstock — dumps in, media database out.
#
# This repo is the code. Its neighbours are:
#
#   ../dump             the Wikimedia dumps, ~129 GB of input
#   ../build            everything derived: the databases, vectors
#   ../wikidata.db      the build-time resolver cache, discardable
#
# Every target is resumable and none of them are incremental: the parse is a
# linear stream over the whole 26.5 GB dump, so a rebuild reads everything.

# The code repo, the dumps and the derived artifacts are siblings, so these
# point outward. Override any of them on the command line.
BIN     := ./filmstock
DUMPS   ?= ../dump
OUT     ?= ../build
WD      ?= ../wikidata.db
WORKERS ?= 18

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

.PHONY: build test dumps resolver extract verify web dist clean-out help

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

## extract    dumps -> the databases (resolver phases included)
extract: build
	@test -s $(WD) || { echo "no $(WD) — run 'make resolver' first"; exit 1; }
	$(BIN) extract -dumps $(DUMPS) -db $(OUT)/filmstock.db \
	  -cache $(WD) -workers $(WORKERS)
	@$(MAKE) --no-print-directory verify

## verify     print what actually landed in the database
verify:
	@sqlite3 $(OUT)/filmstock.db \
	  "select 'movies              '||count(*) from movies" \
	  "select 'events              '||count(*) from events" \
	  "select 'television_series   '||count(*) from television_series" \
	  "select 'television_episodes '||count(*) from television_episodes" \
	  "select 'people              '||count(*) from people" \
	  "select '  with qid          '||count(*) from people where qid is not null" \
	  "select 'credits             '||count(*) from credits"
	@echo "raw wikitext leaking into person names (must be 0):"
	@sqlite3 $(OUT)/filmstock.db \
	  "select '  '||count(*) from people where name like '%[[%' or name like '%<br%'"
	@echo "episodes whose series_id has no series row (must be 0 — episode search"
	@echo "joins for the series title instead of storing a copy):"
	@sqlite3 $(OUT)/filmstock.db \
	  "select '  '||count(*) from television_episodes e \
	   left join television_series t on t.id = e.series_id where t.id is null"

## web        the browser
web: build
	./filmstock-web -db $(OUT)/filmstock.db -addr :8080

## dist       compress the databases for distribution (zstd -19)
dist: $(OUT)/filmstock.db
	zstd -19 -T0 -f -k $(OUT)/filmstock.db -o $(OUT)/filmstock.db.zst
	@printf "  %-22s %s\n" "filmstock.db"     "$$(du -h --apparent-size $(OUT)/filmstock.db     | cut -f1)"
	@printf "  %-22s %s\n" "filmstock.db.zst" "$$(du -h --apparent-size $(OUT)/filmstock.db.zst | cut -f1)"

## clean-out  delete the derived artifacts; keeps the dumps
clean-out:
	rm -rf $(OUT)
