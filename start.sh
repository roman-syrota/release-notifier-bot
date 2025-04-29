#!/usr/bin/bash
echo "Compiling ..."
go build -o bot cmd/bot/main.go
echo "Startig ..."
nohup ./bot > ./bot.log 2>&1 &
echo "Started, log file bot.log"