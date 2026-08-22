# mediadb — dumps in, media database out.
#
# Every target is resumable and none of them are incremental: the parse is a
# linear stream over the whole 26.5 GB dump, so a rebuild reads everything.

BIN     := ./moviedb
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

.PHONY: build test dumps resolver extract index serve clean-out help

help:
	@sed -n 's/^##//p' $(MAKEFILE_LIST)

## build      compile the single binary -> ./moviedb
build:
	cd src && go build -o ../moviedb .

## test       run the parser regression tests
test:
	cd src && go test ./...

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

## serve      web UI on :8080
serve: build
	$(BIN) serve -db $(OUT)/search.db -movies $(OUT)/movies -television $(OUT)/television \
	  -events $(OUT)/events -addr :8080

## clean-out  delete the regenerated outputs; keeps dumps and the resolver cache
clean-out:
	rm -rf $(OUT)
