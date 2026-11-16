ARTICLES ?= articles/obsidian
ARTICLE  ?= 00-skeleton
UNTIL    ?= 2026-11-10
CHROME   := docker run --rm -u $(shell id -u):$(shell id -g) -v "$(CURDIR)/$(ARTICLES)/$(ARTICLE)/visuals":/v zenika/alpine-chrome:124 --no-sandbox --hide-scrollbars --force-device-scale-factor=2

.PHONY: up down logs seed seed-full seed-brief-01 bench-01 charts-01 explain-01 stats visuals test vet

up:            ## the whole company on a laptop
	docker compose up -d --build
	@echo "ops dashboard: http://localhost:8080  booking-api: :8081  tracking-ingest: :8082"

down:
	docker compose down

logs:
	docker compose logs -f booking-api tracking-ingest

seed:          ## 10% volume, ~30 s
	go run ./sim/geksim seed -reset -until $(UNTIL)

seed-full:     ## full size, ~3.5 min
	go run ./sim/geksim seed -reset -scale 1 -until $(UNTIL)

seed-brief-01: ## full size with 18 months of GPS pings: brief 1's "before" database (~25 min)
	go run ./sim/geksim seed -reset -scale 1 -until $(UNTIL) -ping-days 550

B01 := $(ARTICLES)/01-tracking-page
RUN ?= before
bench-01:      ## replay tracking load hour by hour on Tue 10 Nov 2026 (needs make up; RUN=before|index|after)
	@rm -f $(B01)/data/$(RUN)/bench.csv
	@for h in 09 10 11 12 13 14 15 16 17 18 19; do \
	  go run ./sim/geksim bench -at 2026-11-10T$$h:00 -run $(RUN) -out $(B01) || exit 1; \
	done
	go run ./sim/geksim bench-charts -out $(B01)

charts-01:     ## redraw brief 1's charts from the bench data
	go run ./sim/geksim bench-charts -out $(B01)

LABEL ?= before
explain-01:    ## EXPLAIN ANALYZE the tracking queries into the article data (LABEL=before|index|after)
	docker compose exec -T postgres psql -U gekgo -d gekgo -X $(if $(filter after,$(LABEL)),-v after=1) \
	  < briefs/01-tracking-page/explain.sql > $(B01)/data/explain-$(LABEL).txt
	@cat $(B01)/data/explain-$(LABEL).txt

stats:         ## measured numbers + chart SVGs for an article
	go run ./sim/geksim stats -out $(ARTICLES)/$(ARTICLE)

visuals:       ## render every SVG in the article to a 2x PNG for Medium
	@cd $(ARTICLES)/$(ARTICLE)/visuals && for f in *.svg; do \
	  size=$$(sed -nE 's/.*viewBox="0 0 ([0-9]+) ([0-9]+)".*/\1,\2/p' $$f | head -1); \
	  $(CHROME) --window-size=$$size --screenshot=/v/$${f%.svg}.png file:///v/$$f >/dev/null 2>&1 && echo "rendered $${f%.svg}.png"; \
	done

vet:
	go vet ./...

test:
	go test ./...
