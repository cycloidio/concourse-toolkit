#!/bin/bash

export PATH=$PATH:$GOPATH/bin

# Cleanup
rm -rf $GOPATH/bin

# Static build :
#CGO_ENABLED=0 GOOS=linux go build -o bin/concourse-toolkit main.go

# build
go build -o bin/concourse-toolkit main.go
