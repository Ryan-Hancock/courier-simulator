ARTICLE ?= 00-skeleton
UNTIL   ?= 2026-11-10
CHROME  := docker run --rm -u $(shell id -u):$(shell id -g) -v "$(CURDIR)/articles/$(ARTICLE)/visuals":/v zenika/alpine-chrome:124 --no-sandbox --hide-scrollbars --force-device-scale-factor=2

.PHONY: up down logs seed seed-full stats visuals test vet

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

stats:         ## measured numbers + chart SVGs for an article
	go run ./sim/geksim stats -out articles/$(ARTICLE)

visuals:       ## render every SVG in the article to a 2x PNG for Medium
	@cd articles/$(ARTICLE)/visuals && for f in *.svg; do \
	  size=$$(sed -nE 's/.*viewBox="0 0 ([0-9]+) ([0-9]+)".*/\1,\2/p' $$f | head -1); \
	  $(CHROME) --window-size=$$size --screenshot=/v/$${f%.svg}.png file:///v/$$f >/dev/null 2>&1 && echo "rendered $${f%.svg}.png"; \
	done

vet:
	go vet ./...

test:
	go test ./...
