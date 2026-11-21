# pp-email

Send one personalized email per CSV row from a Linux terminal. Choose a named s-nail account, map the CSV columns, and review the messages before sending.

## Build

Use Go 1.27 and install s-nail on your PATH. The adapter was checked with s-nail 14.9.25.

~~~sh
go mod tidy
go build ./cmd/pp-email
./pp-email
~~~

## Prepare a batch

Set up and verify a named account in s-nail first. The app uses s-nail's normal configuration loading, including your `~/.mailrc` or the file set by `MAILRC`. The account must use s-nail's built-in SMTP client with a remote server. Use an SMTP hostname that resolves only to remote addresses throughout the batch. The app checks the hostname before sending, and s-nail resolves it again when connecting.

Use a comma-delimited UTF-8 CSV with a header row. Each data row supplies one email. Column names can vary. Map the recipient name, plain email address, subject, and body by column position. Quote fields that contain commas or actual line breaks.

The plain-text body supports `\n` for a newline, `\r` for a carriage return, `\t` for a horizontal tab, and `\v` for a vertical tab. Double the backslash to keep an escape as literal text, such as `\\t` for a literal `\t`. The preview shows CRLF as a line break, horizontal tabs as four spaces, and standalone carriage returns and vertical tabs as `␍` and `␋`.

~~~csv
Contact,Email address,Topic,Message
Ada,ada@example.invalid,Shipping update,"Hi Ada,\n\nYour order is ready."
Sam,sam@example.invalid,Appointment reminder,"Hi Sam, your appointment is tomorrow."
~~~

1. Open **New batch** and choose an account and CSV. Use the arrow keys and Enter to navigate. Press `?` for shortcuts on the current screen.
2. Map the four fields using the sample values. In **Review**, press `/` to find a message by name, email address, subject, or source line. Press Tab to open or focus the preview, then use the arrow keys or Page Up and Page Down to scroll. Search filters the preview list. The full batch size stays visible.
3. Press Enter to review the account, file, message count, and send limits. Press S to send. During sending, Esc stops after the current submission, or before the next one if the app is waiting for the send limit.

## Optional send limits

Create `~/.config/pp-email/limits.json` to set the sending rate and rolling 24-hour budget for named s-nail accounts. If `XDG_CONFIG_HOME` is set, use its `pp-email` subdirectory instead. Use your exact account names and set both fields to positive integers.

~~~json
{
  "gmail": { "messages_per_minute": 6, "max_per_24_hours": 100 },
  "brevo": { "messages_per_minute": 12, "max_per_24_hours": 200 },
  "ses": { "messages_per_minute": 30, "max_per_24_hours": 500 }
}
~~~

Choose values for your own provider quotas and sending volume. Confirmation shows the configured rate and remaining budget. A batch must fit in full before it can start. Press Esc during a pacing wait to stop before the next message. Accounts without an entry send as usual.

The budget counts attempts made by pp-email while the account has limits configured, including unconfirmed submissions. Leave room for messages sent through the same account from other apps. The attempt history persists under `$XDG_STATE_HOME/pp-email` or `~/.local/state/pp-email`. Restart pp-email after editing the configuration.

## Reports and recovery

Press `2` to open **Reports**. Search run history by account or CSV filename and press Enter to open a run. Use Left and Right to filter outcomes and `/` to search for a source line. Press Tab to read a row's diagnostic, I to open run details, or E to export the full run as CSV. Choose a new export path for each file.

History starts with runs finalized after this upgrade. Reports are stored in owner-only files under `$XDG_STATE_HOME/pp-email/reports` or `~/.local/state/pp-email/reports`. Row details include source line, outcome, and diagnostic. Recipient names, addresses, subjects, and bodies stay in the source and recovery CSVs.

The results show accepted submissions, unconfirmed submissions, and unattempted rows. Acceptance means the SMTP server accepted the submission. When s-nail reports an unconfirmed outcome, the app saves the original row in an owner-only `pp-email-failures-*.csv` beside the source file and continues when possible. Review each unconfirmed row before resending, since a timeout or interrupted connection can leave acceptance unknown. Open a recovery CSV as a new batch after checking the outcomes and addressing the cause.

The results identify source lines that could not be saved and tell you if the recovery file needs inspection. Runs without recovery rows normally remove the header-only file.

## Checks

~~~sh
go test ./...
go vet ./...
PP_EMAIL_TEST_SNAIL=1 go test ./internal/smail
~~~

The opt-in s-nail test uses a synthetic configuration and dry-run mode. Use a dedicated remote SMTP account with controlled recipients to test actual acceptance and rejection.
