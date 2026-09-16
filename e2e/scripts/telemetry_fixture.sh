#!/bin/sh
set -eu

printf 'Do you want me to run this telemetry fixture?\n'
IFS= read -r answer
printf '{"token":"telemetry private value"}\n'
printf 'TELEMETRY_FIXTURE_ANSWER=%s\n' "$answer"
