#!/bin/sh

set -x

export GOPATH=$PWD
export PATH=$PATH:$GOPATH/bin

# Get concourse version from tag
export VERSION=$(cat TAG)

go version

# Get depends
rm -rf concourse pkg/ src/ bin/
wget -q -O concourse.tar.gz https://bosh.io/d/github.com/concourse/concourse?v=${VERSION}
mkdir -p concourse && tar xf concourse.tar.gz -C concourse
mkdir -p src && tar xf concourse/packages/atc.tgz -C src/
go get "github.com/spf13/cobra"
go get "github.com/spf13/viper"
rm -rf concourse concourse.tar.gz

# Static build :
CGO_ENABLED=0 GOOS=linux go build -o bin/concourse-toolkit main.go
