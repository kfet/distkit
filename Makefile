.PHONY: all fmt vet test cover check clean

all: check

fmt:
	@gofmt -l -w .

vet:
	@go vet ./...

test:
	@go test ./... -count=1

cover:
	@go test ./... -coverprofile=coverage.out
	@go tool cover -func=coverage.out | tail -1

# Everything CI runs. `gofmt -l` must print nothing: a formatting difference
# is a diff nobody reviewed.
check:
	@test -z "$$(gofmt -l . | tee /dev/stderr)" || { echo "gofmt: files need formatting"; exit 1; }
	@go vet ./...
	@go test ./... -count=1
	@echo "✓ check"

clean:
	@rm -f coverage.out
