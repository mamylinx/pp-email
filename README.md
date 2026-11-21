<p align="center">
  <img src="docs/assets/pp-email-logo.png" alt="pp-email logo with a terminal prompt inside an envelope" width="420">
</p>

<h3 align="center">pp-email - Send hyperpersonalized emails from CSV on Linux</h3>

**Give every recipient their own subject and message.** Each CSV row holds the complete text for one email. You can write entirely different messages for people in the same batch, right down to the subject line.

Write the messages in your spreadsheet and let pp-email handle sending them one by one. From your Linux terminal, map the columns once, preview what each recipient will see, and send the batch through your existing s-nail SMTP account.

[**Quickstart**](#quickstart) · [SMTP setup](#set-up-s-nail-in-mailrc) · [CSV format](#csv-format) · [Send limits](#set-smtp-send-limits) · [Reports and recovery](#reports-and-recovery) · [Contributing](#contributing)

## Example

The same CSV can send Ada a shipping update and Sam an appointment reminder.

```csv
Contact,Email address,Topic,Message
Ada,ada@example.invalid,Shipping update,"Hi Ada,\n\nYour order is ready."
Sam,sam@example.invalid,Appointment reminder,"Hi Sam, your appointment is tomorrow."
```

Map `Contact` to the recipient name, `Email address` to the recipient address, `Topic` to the subject, and `Message` to the body. In **Review**, Ada’s message appears as:

```text
To: Ada <ada@example.invalid>
Subject: Shipping update

Hi Ada,

Your order is ready.
```

## Quickstart

### Requirements

- **Linux** with an interactive terminal.
- **Go 1.27 or newer** to build pp-email from source.
- **s-nail** installed and on your `PATH`. Check with `s-nail -V`. pp-email was tested with s-nail 14.9.25.
- A **named s-nail account** configured to submit email directly to a remote SMTP server.

### Build and send a batch

1. **Build and launch** from the repository root after [verifying your s-nail account](#test-your-smtp-account).

   ```sh
   go build ./cmd/pp-email
   ./pp-email
   ```

2. **Choose and map a CSV.** Save the [example](#example) as `sample.csv` in the repository root. If you plan to send a live test, replace the example addresses with inboxes you control before loading the file. Press **1** for **New batch** and select your account and file with the arrow keys and **Enter**. On the mapping screen, use the sample values to pick a column for each of the four fields.
3. **Preview before sending.** In **Review**, press **/** to search by name, address, subject, or source line. Press **Tab** to open or focus the preview and use the arrow keys or **Page Up** and **Page Down** to scroll. Search filters the preview list, and the full batch size stays visible.
4. **Confirm the batch.** Press **Enter** to check the account, CSV, message count, and send limits. If you’re using the sample addresses, stop here. Press **S** on the confirmation screen to send a live batch. Press **Esc** to stop after the current submission or before the next one if pp-email is waiting at a send limit.

Press **?** on any screen to see its shortcuts.

## Set up s-nail in `.mailrc`

Add a named `account` to `~/.mailrc`, or edit the file pointed to by `MAILRC` if you already use one. If you have a working named account that uses s-nail’s built-in remote SMTP client, select that account in pp-email.

This example uses implicit TLS on port 465 and password authentication. Replace the example hostname, login, sender address, and password with the settings from your mail provider. Keep the configuration outside the repository and set its permissions to `600` before storing credentials.

```text
account gmail {
  set v15-compat
  set sendwait
  set mta=smtps://<MAIL-ACCOUNT-NAME>%40gmail.com:<YOUR-GOOGLE-APP-PASSWORD>@smtp.gmail.com:465
  set smtp-auth=login
  set from="<MAIL-ACCOUNT-NAME>@gmail.com"
  set tls-verify=warn
}

```

For a provider that requires STARTTLS on port 587, use `set mta=submission://smtp.example.invalid:587 smtp-use-starttls` in place of the `mta` line. Set `smtp-auth` to the method your provider requires. Use a hostname that resolves only to remote addresses throughout the batch. pp-email checks it before sending, and s-nail resolves it again when connecting. [s-nail 14.9.25 manual](https://manpages.debian.org/trixie/s-nail/s-nail.1.en.html)

### SMTP providers

- **Gmail** uses `smtp.gmail.com`. For the password-based example above, use a Google app password if your account allows it. [Gmail SMTP setup](https://knowledge.workspace.google.com/admin/gmail/send-email-from-a-printer-scanner-or-app) · [Google app passwords](https://support.google.com/mail/answer/185833)
- **Brevo** uses `smtp-relay.brevo.com`. Use the SMTP login and SMTP key from your Brevo account. [Brevo SMTP setup](https://help.brevo.com/hc/en-us/articles/7924908994450-Send-transactional-emails-using-Brevo-SMTP)
- **Amazon SES** uses a regional SMTP endpoint and SMTP credentials for that AWS Region. Use a sender identity you’ve verified in SES. [Amazon SES SMTP setup](https://docs.aws.amazon.com/ses/latest/dg/send-email-smtp.html)

### Test your SMTP account

Test the account with s-nail before using it in pp-email. Replace `gmail` with your account name if it differs, and use a recipient address you control.

```sh
printf '%s\n' 'Testing my s-nail SMTP account.' |
  s-nail -A gmail -S sendwait -s 'SMTP test' recipient@example.invalid
```

Check that the message arrives. If you use a custom configuration file, set `MAILRC` for both the s-nail test and pp-email.

## CSV format

Use a comma-delimited UTF-8 file with a header row. Each data row supplies one email. Headers can have any names because mapping uses column positions. Use a plain email address in the address column, and quote fields with commas or actual line breaks.

The plain-text body also supports these escapes:

| Escape | Result |
| --- | --- |
| `\n` | Newline |
| `\r` | Carriage return |
| `\t` | Horizontal tab |
| `\v` | Vertical tab |

Double the backslash to keep an escape as literal text, such as `\\t` for a literal `\t`. The preview displays CRLF as a line break, a horizontal tab as four spaces, and standalone carriage returns and vertical tabs as `␍` and `␋`.

## Set SMTP send limits

For each named account, you can set a per-minute rate and a rolling 24-hour budget. Create `~/.config/pp-email/limits.json`. If `XDG_CONFIG_HOME` is set, create `$XDG_CONFIG_HOME/pp-email/limits.json` instead. Use your exact s-nail account names and set both values to positive integers.

```json
{
   "gmail": { "messages_per_minute": 6, "max_per_24_hours": 100 },
   "brevo": { "messages_per_minute": 12, "max_per_24_hours": 200 },
   "ses": { "messages_per_minute": 30, "max_per_24_hours": 500 }
}
```

Restart pp-email after editing the file. On the **Send** screen, check **Pacing** and **Send budget** for the selected account. The full batch must fit within the remaining budget before it starts. Accounts without an entry show **No limits configured**.

The budget counts pp-email attempts while limits are configured for the account, including unconfirmed submissions. Attempt history persists under `$XDG_STATE_HOME/pp-email` or `~/.local/state/pp-email`. Choose values based on your provider’s quotas and leave room for messages sent through the same account from other apps.

## Reports and recovery

Press **2** for **Reports** and search by account or CSV filename. Open a run with **Enter**. Filter outcomes with **Left** and **Right**, search by source line with **/**, and read the selected row’s diagnostic with **Tab**. Press **I** for run details or **E** to export the full report to a new CSV path.

| Outcome | Meaning |
| --- | --- |
| **Accepted** | The SMTP server accepted the submission. Inbox delivery is a separate step. |
| **Unconfirmed** | pp-email could not confirm that the server accepted the submission. Check before resending to avoid duplicates. |
| **Unattempted** | The run ended before pp-email tried to send this row. |

pp-email saves reports in owner-only files under `$XDG_STATE_HOME/pp-email/reports` or `~/.local/state/pp-email/reports`. Row details include the source line, outcome, and diagnostic. Recipient names, addresses, subjects, and bodies stay in the source and recovery CSVs. If a report fails to save, it stays available in **Reports** for the rest of the session, even after later runs. Export it before quitting.

For unconfirmed submissions, pp-email writes the original rows to an owner-only `pp-email-failures-*.csv` beside the source CSV and continues when possible. Check the run for rows it could not save or a recovery file that needs inspection. After checking acceptance and addressing the cause, open the recovery CSV as a new batch with the rows you intend to resend. Runs without recovery rows normally remove the header-only file.

## Development checks

```sh
go test ./...
go vet ./...
PP_EMAIL_TEST_SNAIL=1 go test ./internal/smail
```

The opt-in s-mailx test uses a synthetic configuration and dry-run mode. Use a dedicated remote SMTP test account with controlled recipients to verify live acceptance and rejection. [CSV email sender design](docs/designs/csv_email_sender.md)

## Contributing

Contributions are what make the open source community such an amazing place to learn, inspire, and create. Any contributions you make are greatly appreciated.

If you have a suggestion to improve pp-email, fork the repository and open a pull request, or open an issue and use the `enhancement` label if available. If you find the project useful, give it a star. Thanks for contributing!

1. Fork the repository and clone your fork.
2. Create a feature branch with `git checkout -b feature/report-export-preview`.
3. Commit your changes with `git commit -m 'feat(tui): preview report exports'`.
4. Push your branch with `git push origin feature/report-export-preview`.
5. Open a pull request.

Keep each change focused and run the [development checks](#development-checks) before submitting it. For bug reports and test fixtures, use synthetic CSV rows and keep recipient data and SMTP credentials out of Git.

### Commit message style

Use `type: summary` or `type(scope): summary`. Add a short scope such as `tui`, `csv`, or `smtp` when it identifies the part of the app you changed.

| Type | Use for |
| --- | --- |
| `feat` | New user-facing behavior |
| `fix` | Bug fixes |
| `docs` | Documentation changes |
| `refactor` | Code changes that preserve behavior |
| `test` | Test additions or updates |
| `chore` | Repository maintenance or tooling |

Start the summary with a lowercase imperative verb such as `add`, `fix`, or `explain`. Describe the specific change, aim for 72 characters or fewer for the full subject line, and omit the final period. When the reason needs more context, leave a blank line and explain it in the commit body.

```text
feat(tui): show the selected account during review
fix: preserve the send budget when a session closes twice
docs: explain how to verify an SMTP account
```

## Acknowledgments

Thanks to the projects that helped make pp-email possible.

- **S-mailx (formerly s-nail)** handles SMTP submission through your existing mail accounts. [s-nail project](https://git.sdaoden.eu/browse/s-nail.git)
- **Google’s Elephant-Goldfish Model (EGM)** provided a workflow for drafting the design and reviewing it with fresh eyes. [google/elephant-goldfish-model](https://github.com/google/elephant-goldfish-model)
- **samber/cc-skills-golang** provided practical guidance for Go development and documentation. [samber/cc-skills-golang](https://github.com/samber/cc-skills-golang)

## License

pp-email is licensed under the [MIT License](LICENSE).
