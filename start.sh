#!/usr/bin/bash
go build -o bot cmd/bot/main.go
nohup ./bot > ./bot.log 2>&1 &