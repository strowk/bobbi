#!/bin/bash

go build -ldflags="-X main.gitHash=$(git rev-parse HEAD) -X main.buildDate=$(date -u +%Y-%m-%dT%H:%M:%SZ)" -o solution-deliverable/bobbi .