# go-sendemail

Send an e-mail from a script or a cron job: one static binary, Go standard library only, no config file.

```sh
df -h / | go-sendemail -from "cron@example.com" -to "ops@example.com" \
  -subject "Disk report $(hostname)" -body-file - -attach /var/log/backup.log
```

It writes a standards-compliant message (MIME, UTF-8 subject and body, attachments, `Date` and `Message-ID`
headers) and hands it to an SMTP server, by default the relay on `localhost:25`.

## Install

```sh
go install github.com/vitorhmcorreia/go-sendemail@latest
```

Or build a static Linux binary from a checkout with `./build.sh` (`CGO_ENABLED=0`, version from `git describe`).

## Usage

| Flag | Meaning |
|---|---|
| `-from` | sender, e.g. `'Backups <backup@example.com>'` (required) |
| `-to`, `-cc`, `-bcc` | comma-separated recipients; Bcc goes in the envelope only, never in the headers |
| `-subject` | subject |
| `-subject-file` | file whose first line is the subject (wins over `-subject`) |
| `-body-file` | plain-text body; `-` reads standard input (required) |
| `-attach` | comma-separated files to attach |
| `-recipients-file` | `To=` / `Cc=` / `Bcc=` lines, used when no recipient flag is given |
| `-smtp-host`, `-smtp-port` | SMTP server (default `localhost`, `25`) |
| `-tls` | `auto` (default): STARTTLS when the server offers it, certificate verified; `off`: never |
| `-log-file` | append log lines here instead of standard error |
| `-version` | print the version |

Recipients come from the flags; if none are given, from `-recipients-file`; if still none, from the environment
variables `EMAIL_TO`, `EMAIL_CC` and `EMAIL_BCC`.

A recipients file:

```
# on-call team
To=ops@example.com, oncall@example.com
Cc=lead@example.com
Bcc=audit@example.com
```

### In scripts

- Exit code 0 means the server accepted the message; anything else is 1, with the reason on standard error.
  Standard output stays empty.
- The log line names the subject, the number of recipients and the server, never the body.

```sh
if ! ./backup.sh > /tmp/backup.out 2>&1; then
  go-sendemail -from "backup@example.com" -to "ops@example.com" \
    -subject "Backup FAILED on $(hostname)" -body-file /tmp/backup.out || logger -t backup "alert mail failed"
fi
```

## Limits

- No SMTP authentication: it is meant for a local or trusted relay (Postfix, a relay container, a smart host on
  your network). The relay does the authenticated delivery onwards.
- With `-tls auto` the server certificate is checked against `-smtp-host`. A local relay with a self-signed
  certificate needs `-tls off`.
- Plain-text body only (no HTML part).

## Tests

```sh
go test ./...
```

The tests check the message format (CRLF line endings, lines under 78 characters, attachments decode back
unchanged, no Bcc in the headers, line breaks in a subject or address rejected) and send through a fake SMTP
server in the test itself, so nothing leaves the machine.

## Licence

MIT, see [LICENSE](LICENSE).
