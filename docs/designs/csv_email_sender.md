# CSV Email Sender

- **Author** mamylinx
- **Status** Approved
- **Date** 2026-11-21

## Overview

A Linux terminal app sends personalized batches from a CSV using s-nail's built-in SMTP client. Each data row provides one recipient and one message. The sender chooses a named account configured for direct SMTP to a remote server, maps the CSV columns, and reviews the batch before sending.

## The Problem

A CSV export can contain everything needed for a batch of emails, but its headers depend on where the file came from. A sender needs to match each column to the right message field and check that each recipient gets the intended subject and body. If a send fails partway through the batch, the sender needs the original failed row so they can follow up without reconstructing it from the source file.

## Goals

- Let the sender choose a CSV with headers and map its columns to recipient name, recipient email address, subject, and body. For example, the `Recipient` column can supply the recipient name.
- Let the sender choose one named account for the batch from the configuration used by s-nail. The sender sets up and verifies remote SMTP for that account in s-nail before using the app.
- Show the chosen account and message count before confirmation. Let the sender browse previews of the recipient name, address, subject, and body for individual rows.
- Submit one message per data row through s-nail after the sender confirms. When an individual send fails, write its original row to a batch-specific failure CSV and continue with the next recipient.
- Preserve the original headers, column order, and row values in the failure CSV. Show the batch totals and the failure file path at the end.

## Non-Goals

- SMTP setup, credential management, or editing `.mailrc` from the TUI.
- Automatic retries or a persistent queue of failed rows.

## Critical User Journeys

### Review and send a batch

The sender has a working named SMTP account in s-nail and a CSV with message data. They choose the account and file, map the four message fields, and inspect the preview. The TUI shows the selected account and number of messages. On confirmation, the batch uses the same mapped values the sender reviewed and s-nail connects directly to the account's remote SMTP server.

### Recover a failed row

The transport rejects one message partway through the batch. The sender sees that the app continues with the following row. At the end, the TUI shows the results and the path to a CSV containing the rejected row, with its original headers and values. The sender can fix the cause and open that CSV as a new batch.

## Technical Plan (Architecture)

```text
Active s-nail config -> account discovery -> account picker
                                                |
CSV -> import and mapping -> validation and preview
                                      |
                              sender confirms
                                      |
                         batch runner -> s-nail SMTP client -> remote SMTP server
                              |
                              +-> failure CSV and batch totals
```

### Account discovery

Ask s-nail to load its configuration and run its `account` listing command. Discovery and sending use the same configuration loading behavior, including the active personal config at `~/.mailrc` or the path set in `MAILRC`. S-nail resolves included files and conditional commands. The account picker receives only user-defined names, and the batch runner passes the selected name to s-nail for each submission. If discovery fails or produces no named accounts, the TUI reports the cause and prevents sending. [S-nail manual](https://www.sdaoden.eu/code-nail.html)

The listing includes full account definitions and can contain credentials. Extract account names from the process output and discard the raw listing without displaying or logging it. Verify the output format for the supported s-nail versions.

### Import, map, and preview

The importer reads CSV records with a CSV parser. It retains each record's source position and original field values, including unmapped columns. The mapping screen shows the source headers and their positions so repeated headers can be distinguished. The preview and sender use the same mapped message data. Before confirmation, the TUI reports missing mappings and invalid or missing recipient addresses by record and blocks sending until they are resolved.

### Submit and record the outcome

Only accounts whose effective transport uses s-nail's built-in SMTP client to reach a remote server can submit a batch. Check the selected account's effective transport before sending. If it falls back to a local mail program, points to a local SMTP server, or cannot be determined, stop and show an account configuration error. This check covers transport selection. SMTP setup and verification remain the sender's responsibility. [S-nail manual](https://www.sdaoden.eu/code-nail.html)

The batch runner submits confirmed rows in CSV order, one at a time. It invokes s-nail directly, supplies one recipient and a subject as message fields, and passes the body as data on standard input. The adapter keeps CSV content from being interpreted as shell or s-nail commands and ensures each row supplies a single delivery address. It waits for the SMTP result before advancing. Use `sendwait` to observe transfer errors and disable s-nail's failed-message copy for app invocations so the failure CSV serves as the recovery record. A successful submission records acceptance by the remote SMTP server. [S-nail manual](https://www.sdaoden.eu/code-nail.html)

When s-nail reports a failure, the batch runner writes the original row to the run's failure CSV and continues. Use a distinct, owner-only file for each run. The TUI shows the submission and failure counts and the file path when the batch finishes. If a failure row cannot be written, stop before attempting another recipient and show the affected row and the number of rows still pending. The failure CSV contains personal data, so keep raw mail transport diagnostics and account settings out of it.

### Verification before implementation

A check with s-nail 14.9.25 used a temporary config and `/usr/bin/false` to simulate a failed transfer. The `account` command printed full definitions. With `sendwait` enabled, the simulated failure produced exit status 4, and `nosave` suppressed the failed-message copy.

Use a stub of the s-nail process to exercise account selection and batch failure handling. Use a dedicated remote SMTP test account for an end-to-end check of direct submission and SMTP rejection. Include a test that a missing or local transport is rejected before any message is submitted.

## Alternatives Considered

### Parse account declarations directly from `.mailrc`

Reading the file directly can find explicit account names without starting s-nail or running the configuration. Matching the accounts that s-nail actually loads would also require handling included files and conditional commands. The author chose s-nail's account listing, which uses the same configuration rules as sending.

### Route mail through a local mail program or SMTP server

A local relay can manage its own queue and delivery settings. It adds a dependency on local mail infrastructure and changes which component connects to the remote server. The author required s-nail to connect directly to the remote SMTP server configured for the selected account.

### Require fixed CSV headers

Fixed headers can remove a mapping step for files exported from a single template. Existing exports with different column names would need to be edited before import. The author chose column mapping so each sender can use the headers their file already contains.

## Detailed Implementation Plan

Build the Linux executable `pp-email` with Go 1.27, Bubble Tea, and Bubbles. The initial compatibility target is s-nail 14.9.25. Use the installed s-nail to discover accounts and submit mail directly to the selected account's remote SMTP server. [Bubble Tea](https://github.com/charmbracelet/bubbletea) · [Bubbles](https://github.com/charmbracelet/bubbles)

### Target Files to Create

1. **[go.mod](../../go.mod).** Declare the local module `pp_email` with the `go 1.27.0` directive. Pin `charm.land/bubbletea/v2` to v2.1.1 and `charm.land/bubbles/v2` to v2.2.0 so builds select those dependency versions. Use the Go standard library for CSV, child processes, network address checks, and file I/O. [Bubble Tea v2.1.1 module](https://raw.githubusercontent.com/charmbracelet/bubbletea/v2.1.1/go.mod) · [Bubbles v2.2.0 module](https://raw.githubusercontent.com/charmbracelet/bubbles/v2.2.0/go.mod)
2. **[go.sum](../../go.sum).** Generate dependency checksums with `go mod tidy` after the TUI imports are in place.
3. **[cmd/pp-email/main.go](../../cmd/pp-email/main.go).** Resolve the `s-nail` executable and capture the current environment at startup. Start Bubble Tea and route terminal quit and interrupt requests through the model's stop path. Write a safe error to stderr and return a nonzero exit status if the executable is missing or the terminal program cannot start.
4. **[internal/csvdata/csv.go](../../internal/csvdata/csv.go).** Read the selected file once. Parse and validate the snapshot using the CSV contract below. Return the mapped message and source record together for preview, dispatch, and recovery.
5. **[internal/smail/smail.go](../../internal/smail/smail.go).** Own the s-nail invocations, endpoint validation, process cleanup, and conversion from child observations to submission results. Use the s-nail command and outcome contracts below.
6. **[internal/batch/batch.go](../../internal/batch/batch.go).** Run the confirmed messages sequentially in CSV order. Check the selected account and open the recovery file before the first send. Apply the outcome table, recovery-file protocol, and stop behavior below.
7. **[internal/tui/model.go](../../internal/tui/model.go).** Use the Bubbles file picker to choose a CSV, a table to browse rows, and a scrollable pane for the body. Let the user choose an account and all four column positions. Show position labels for repeated headers and validation errors by source line. Keep confirmation disabled while validation errors remain. Show the chosen account, message count, and an explicit `Send N emails` action. Render imported values as terminal-safe text. Mark the batch active before starting its worker. Show the result counts, source lines for unconfirmed or unrecorded rows, the first unattempted line after a stop, and the recovery path and status when available.
8. **[internal/csvdata/csv_test.go](../../internal/csvdata/csv_test.go).** Test arbitrary and repeated headers, mapping by position, encoding and length validation, invalid addresses, control characters, and source line numbering. Use an exact-byte fixture with a UTF-8 BOM, leading and intervening blank lines, CRLF, and a quoted multiline body. Reopen the result and compare its header and selected record bytes with the input.
9. **[internal/smail/smail_test.go](../../internal/smail/smail_test.go).** Test exact invocation arguments and body data with a helper process. Cover output parsing, redaction, the outcome table, network endpoint checks, and process group cleanup. Add the opt-in installed-binary checks in Verification Plan.
10. **[internal/batch/batch_test.go](../../internal/batch/batch_test.go).** Use a fake adapter and a temporary directory to verify row order, write-before-advance, continuation after an unconfirmed result, and stop during startup or while a send is in flight. Test short writes, rollback failure, close failure, count preservation, and deletion of the header-only file after a normal run.
11. **[internal/tui/model_test.go](../../internal/tui/model_test.go).** Feed keyboard and worker messages to the model. Test account selection, position-based mapping, preview, invalid-input feedback, and repeated confirmation while starting. Verify graceful quit and that the result counts, affected lines, and recovery status render.
12. **[README.md](../../README.md).** Explain the Go 1.27 build, installed s-nail requirement, named remote SMTP account, CSV format, mapping, and result screen. Tell senders to use an SMTP hostname that resolves only to remote addresses throughout the batch. The app checks the hostname before sending, and s-nail resolves it again when connecting. Explain that the recovery CSV contains original rows with unconfirmed submission and that senders should check acceptance before resending. Include a small synthetic CSV example.

### Package contracts and stop behavior

| Owner | Data and operations |
| --- | --- |
| `csvdata` | `Snapshot` holds the source path, original bytes, decoded headers, raw header span, BOM length, and ordered records. Each `Record` holds a one-based physical source line, decoded fields, and its source byte span. Each `Message` holds its source record, formatted recipient, subject, and body. |
| `smail` | `ListAccounts` returns ordered names or a safe discovery error. `CheckAccount` returns an approved endpoint for a named account or a safe configuration error. `Send` takes that account and endpoint with one prepared message and returns `SendResult`, which carries an outcome, diagnostic reason, and whether the batch must stop. |
| `batch` | `Run` takes the confirmed account, snapshot, prepared messages, adapter, recovery writer, and a receive-only stop signal. `Progress` carries counts and the current source line. `Result` carries final counts, unconfirmed and unrecorded source lines, the first unattempted source line when present, and recovery path and status. |
| `tui` | The model owns the stop signal and consumes the worker's ordered event channel, buffered for one event. Each Bubble Tea worker command waits for the next event. The final event contains `Result`. |

Mark the run active before launching the worker. A repeated confirmation while active has no effect. Close the stop signal once on a quit request. During startup, check the signal before the first send. During a send, let that invocation finish or reach its own deadline, record its result, and stop before the next row. The stop signal does not cancel the active process. Keep the model active until it consumes the final result.

### CSV input and original record bytes

Accept comma-delimited UTF-8 CSV with a header and at least one data row. Allow a leading UTF-8 BOM and blank lines between records. Use `encoding/csv.Reader` with strict quoting and a field count fixed by the header. Report invalid UTF-8, missing data, or a parse error, with the physical line when available. [Go CSV package](https://pkg.go.dev/encoding/csv)

Index physical line starts in the original bytes. Skip a leading BOM only in the decoder view. For the header and every data record, find its starting physical line with `FieldPos(0)`. End its raw span at `InputOffset` plus the skipped BOM length. Start at that physical line's byte offset, except that a record starting on the first physical line begins after the BOM. Use these spans so skipped blank lines stay out of the recovery CSV and CRLF bytes inside a quoted field survive decoder normalization. Write the original BOM once, followed by the raw header and each selected raw record.

Map name, email address, subject, and body by column position. Reject control characters in name, address, and subject before trimming surrounding address spaces. Validate a single plain mailbox containing an `@` with `net/mail` and format it with the mapped display name. Allow blank name, subject, and body values. Allow carriage returns, line feeds, horizontal tabs, and vertical tabs in the body. Reject other control characters, including NUL. Decode `\r`, `\n`, `\t`, and `\v` in the body. A doubled backslash keeps the escape as literal text. [Go mail package](https://pkg.go.dev/net/mail)

### S-nail commands and endpoint policy

Run all operations directly via `os/exec` with the same captured environment and normal resource-file loading. The command contract assumes a trusted s-nail configuration that supports the startup operations below. The table shows ordered argument vectors. Square brackets delimit the vector, curly-bracketed values are supplied by the adapter, and each quoted entry is one argument.

~~~text
Discover [-X, \account, -X, "\exit 0", -., pp-email-probe@example.invalid]
Probe    [-A, {account}, -X, 'echo "$PP_EMAIL_PROBE_TAG=$mta"', -X, "\exit 0", -., pp-email-probe@example.invalid]
Send     [-A, {account}, -X, {guard-script}, -s, {subject}, -., {formatted-recipient}]
~~~

Give discovery and probing empty stdin. The startup exit executes before composition. Parse complete top-level blocks in the s-nail 14.9.25 listing, starting with `account <name> {` and finishing at the block's closing brace. Preserve the listed name and order, omit `null`, and report an unrecognized listing or unsuccessful child exit as a discovery error. During a probe, put a fresh hexadecimal tag in `PP_EMAIL_PROBE_TAG` and require a successful exit and one stdout line beginning with that tag and `=`. Read the effective `mta` after the separator. Capture stdout and stderr privately, extract the names or probe result, and discard raw output. [S-nail manual](https://manpages.debian.org/unstable/s-nail/s-nail.1.en.html)

Accept a URL using `smtp`, `smtps`, `submission`, or `submissions` with a host and valid port if explicit. Resolve hostnames before the batch and require a nonempty result. Check all resolved addresses, or the literal address, after unmapping IPv4-mapped IPv6 addresses. Reject loopback, unspecified, and addresses assigned to the current machine. A failed address lookup prevents sending. Store the scheme and authority spelling without credentials. Build an anchored POSIX extended regular expression from escaped endpoint components and permit optional URL user information before the authority. Match the approved authority exactly, including IPv6 brackets and whether the port was written. A changed spelling or newly explicit port requires the sender to start a new batch. S-nail resolves hostnames again when connecting, so the preflight assumes stable remote DNS answers during the run.

For sending, pass the expression through `PP_EMAIL_MTA_RE` and a fresh tag through `PP_EMAIL_PROBE_TAG`. Run the following startup commands after account activation. Supply exactly one recipient and the plain-text body on stdin. Keep raw output private, including diagnostics that may contain credentials or message content.

~~~text
set errexit
wysh if "$mta" =~ "$PP_EMAIL_MTA_RE"
  set sendwait nosave fullnames ttycharset=utf-8 expandaddr=fail,-all,+addr,failinvaddr
  unset autocc autobcc on-compose-enter on-compose-splice on-compose-leave on-compose-cleanup message-inject-head message-inject-tail dot skipempty debug
  echo "$PP_EMAIL_PROBE_TAG READY"
else
  echo "$PP_EMAIL_PROBE_TAG BLOCKED"
  exit 71
endif
~~~

Give discovery and account probes 10 seconds and each send 120 seconds. Start each child in a Linux process group. At the deadline, terminate the group and wait for the child to exit. Treat a s-nail exit as a process observation and evaluate it together with the startup markers.

### Submission outcomes

| Child observation | Row outcome | Recovery CSV | Batch action |
| --- | --- | --- | --- |
| Child never starts, or one matching `BLOCKED` marker and exit 71 | Unattempted | Exclude | Stop and show the first unattempted source line |
| One matching `READY` marker and exit zero | Accepted | Exclude | Continue |
| One matching `READY` marker and nonzero exit, signal, or timeout | Unconfirmed | Include original record | Record the row, then continue |
| Child starts and has no valid startup marker sequence | Unconfirmed | Include original record | Record the row, then stop with a startup error |

Require exactly one valid matching marker and no conflicting marker. Count every source row once. Classify all rows after a stop as unattempted. A nonzero exit can follow a network interruption with uncertain acceptance. Show all unconfirmed source lines and distinguish nonzero exit, timeout, interruption, guard refusal, and process startup errors without displaying raw output. Senders should check acceptance before reusing a recovery row.

### Recovery file

Before the first send, create a unique owner-only `pp-email-failures-*.csv` next to the source CSV. Write and sync the original BOM if present and the raw header. If file creation, header write, or sync fails, end the run before attempting any row.

For an unconfirmed row, append the complete raw record and sync it before advancing. Track the byte offset after the last fully written and synced record. On a short write, write error, or sync error, try to truncate to that offset and sync again. Stop the batch and report the source line that could not be recorded. If rollback and sync succeed, show that complete earlier records are available. If either fails, mark the recovery file unverified and show its path. Mark the file unverified if close fails and preserve the path. These storage failures leave the send counts intact. Remove a header-only file on normal finalization if no rows needed recovery. Preserve a recovery file that contains rows or has an unverified write or close.

### Implementation phases and checkpoints

1. Create the Go module, CSV importer, and s-nail adapter with their tests. Run `go test ./internal/csvdata ./internal/smail`. The CSV byte fixtures, outcome table cases, and timeout cleanup tests must pass.
2. Add the batch runner and its tests. Run `go test ./internal/...`. Require ordered submission, recoverable output, and graceful stop tests to pass.
3. Add the TUI, executable entry point, and README. Add the pinned TUI dependencies to the module and run `go mod tidy`. Run `go test ./...`, `go vet ./...`, and `go build ./cmd/pp-email` on Linux with Go 1.27.x. Verify that one confirmation starts one batch and that progress and results render.

### Verification Plan

- In the default tests, inject the child launcher, hostname lookup, local-address lookup, and recovery writer. Re-execute the Go test binary in a helper mode to control stdout and stderr, status, and descendant PIDs. Test cleanup of both a timed-out parent and its descendant. Exercise ambiguous acceptance, report errors after a send, and the result displayed when a record cannot be saved.
- Run the installed-binary checks with `PP_EMAIL_TEST_SNAIL=1`. Use a private temporary directory, a synthetic `MAILRC`, and `-:u` to load only that personal file. Define two named accounts inside an `if send` block. Set the remote test account's `mta` to `smtps://smtp.example.invalid:465` and `smtp-auth` to `none`. Give the second account a local executable transport value. Confirm both accounts are listed and the probe returns the selected transport. For send checks, add `-d` and keep `debug` set by omitting it from the startup script's `unset` command. Verify `READY` and literal body lines for the remote test account, including lines beginning with a tilde and a period. Verify `BLOCKED` and exit 71 before composition for the local transport.
- Run the live TUI test with a dedicated remote SMTP account and controlled test recipients. Use the remote service's transaction records or mailbox to verify that the first and last rows were accepted. Require an explicit SMTP rejection for the known failing middle row. Verify the run continues and the recovery CSV contains the original header and rejected row bytes.

A dry run with s-nail 14.9.25 selected the send-mode account, emitted the expected guard markers, and preserved lines beginning with a tilde or period as body text. The local transport configuration exited with status 71 before composition.

*Drafted with EGM*
