#!/bin/sh
# Sign every release binary in a directory with the Developer ID certificate.
#
# The certificate arrives as a base64-encoded .p12, imported into a temporary
# keychain that is deleted when the script exits: a runner's login keychain is
# never touched, and the private key does not outlive the build.
#
# Environment:
#   APPLE_CERTIFICATE_P12       base64 of the "Developer ID Application" .p12
#   APPLE_CERTIFICATE_PASSWORD  the .p12 export password
#   APPLE_KEYCHAIN_PASSWORD     any password; guards only the temp keychain
set -eu

bin_dir=${1:?usage: sign-macos.sh <binary directory>}

: "${APPLE_CERTIFICATE_P12:?base64 of the Developer ID .p12}"
: "${APPLE_CERTIFICATE_PASSWORD:?the .p12 export password}"
: "${APPLE_KEYCHAIN_PASSWORD:?any password for the temporary keychain}"

keychain="build-$$.keychain"
p12=$(mktemp)
cleanup() {
	security delete-keychain "$keychain" 2>/dev/null || true
	rm -f "$p12"
}
trap cleanup EXIT

security create-keychain -p "$APPLE_KEYCHAIN_PASSWORD" "$keychain"
security set-keychain-settings -lut 21600 "$keychain"
security unlock-keychain -p "$APPLE_KEYCHAIN_PASSWORD" "$keychain"

# Prepend to the search list rather than replace the default keychain: codesign
# finds the identity, and the runner's keychain state survives the job.
existing=$(security list-keychains -d user | sed 's/"//g')
# shellcheck disable=SC2086
security list-keychains -d user -s "$keychain" $existing

echo "$APPLE_CERTIFICATE_P12" | base64 --decode >"$p12"
security import "$p12" -k "$keychain" -P "$APPLE_CERTIFICATE_PASSWORD" -T /usr/bin/codesign

# codesign must reach the private key without a UI prompt; a runner has no user
# to answer one.
security set-key-partition-list -S apple-tool:,apple: -s -k "$APPLE_KEYCHAIN_PASSWORD" "$keychain"

identity=$(security find-identity -v -p codesigning "$keychain" |
	sed -n 's/.*"\(Developer ID Application: [^"]*\)".*/\1/p' | head -n 1)
if [ -z "$identity" ]; then
	echo "no Developer ID Application identity in the certificate" >&2
	exit 1
fi
echo "signing as: $identity"

# --options runtime is the hardened runtime, which notarization requires;
# --timestamp embeds Apple's timestamp so the signature outlives the
# certificate.
for bin in "$bin_dir"/hive "$bin_dir"/hive-plugin-*; do
	codesign --sign "$identity" --options runtime --timestamp --force "$bin"
	codesign --verify --verbose=2 "$bin"
done
