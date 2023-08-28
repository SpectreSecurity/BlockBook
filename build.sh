#!/bin/bash

export CGO_CFLAGS="-I/usr/local/include"
#export CGO_LDFLAGS="-L/usr/local/lib -lrocksdb -lstdc++ -lm -lz -ldl -lbz2 -lsnappy -llz4 -lzstd"
export CGO_LDFLAGS='-L/usr/local/lib -L/usr/local/opt/zlib/lib -g -O2 -lrocksdb -lstdc++ -lm -llz4 -lbz2 -lsnappy -lzstd -ljemalloc -lz'

#go get -v -x blockbook/db

#go build -ldflags "-X google.golang.org/protobuf/reflect/protoregistry.conflictPolicy=warn"

go build -o bin/blockbook blockbook.go
