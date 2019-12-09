#!/bin/sh

set -x

export PATH=$PATH:$GOPATH/bin

# Get concourse version from tag
export VERSION=$(cat TAG)

go version

# Static build :
GOFLAGS=-mod=vendor CGO_ENABLED=0 GOOS=linux go build -o /go/bin/concourse-toolkit main.go
