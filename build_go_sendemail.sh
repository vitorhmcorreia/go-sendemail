#!/bin/bash

echo -e "Building go_sendemail..."
#go build -o go_sendemail go_sendemail.go

## For a completely static binary on Linux (no CGO), ensure CGO_ENABLED=0:
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o go_sendemail go_sendemail.go

echo -e "Build complete."

echo -e "You can run the program using ./go_sendemail with appropriate flags."
#echo -e "e.g., ./go_sendemail -from "sender@example.com" -to "alice@example.com" -subject "Test" -body-file "body.txt""

echo -e "./go_sendemail \
  -from "sender@example.com" \
  -to "alice@example.com,bob@example.com" \
  -cc "carol@example.com" \
  -bcc "dave@example.com" \
  -subject-file "subject.txt" \
  -body-file "body.txt" \
  -attach "/path/to/file1.txt,/path/to/image.png" \
  -recipients-file "recipients.txt" \
  -smtp-host "localhost" \
-smtp-port "25""