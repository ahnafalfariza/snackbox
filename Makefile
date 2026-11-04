.PHONY: run test docker

run:
	go run ./cmd/snackbox

test:
	go test ./...

docker:
	docker compose up --build
