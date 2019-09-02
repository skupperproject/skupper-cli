VERSION := $(shell git describe --tags --dirty=-modified)

all: build

build:
	go build -ldflags="-X main.version=${VERSION}"  -o skupper cmd/skupper.go

clean:
	rm -rf skupper release

deps:
	dep ensure

package: release/windows.zip release/darwin.zip release/linux.tgz

release/linux.tgz: release/linux/skupper
	tar -czf release/linux.tgz -C release/linux/ skupper

release/linux/skupper: cmd/skupper.go
	GOOS=linux GOARCH=amd64 go build -o release/linux/skupper cmd/skupper.go

release/windows/skupper: cmd/skupper.go
	GOOS=windows GOARCH=amd64 go build -o release/windows/skupper cmd/skupper.go

release/windows.zip: release/windows/skupper
	zip -D release/windows.zip release/windows/skupper

release/darwin/skupper: cmd/skupper.go
	GOOS=darwin GOARCH=amd64 go build -o release/darwin/skupper cmd/skupper.go

release/darwin.zip: release/darwin/skupper
	zip -D release/darwin.zip release/darwin/skupper



