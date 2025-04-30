#!/bin/bash

LOG_FILE="./bot.log"

while true; do
  echo "[$(date)] Starting bot..." >> $LOG_FILE
  ./bot >> $LOG_FILE 2>&1
  
  EXIT_CODE=$?
  echo "[$(date)] Bot exited with code $EXIT_CODE, restarting in 5 seconds..." >> $LOG_FILE
  sleep 5
done
