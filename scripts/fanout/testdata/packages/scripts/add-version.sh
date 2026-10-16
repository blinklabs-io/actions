#!/usr/bin/env bash
# Creates a new package version by copying the latest and updating version fields
# Usage: ./scripts/add-version.sh <package-name> <new-version>
# Example: ./scripts/add-version.sh dolos 0.34.0

set -euo pipefail

if [[ $# -ne 2 ]]; then
  echo "Usage: $0 <package-name> <new-version>"
  echo "Example: $0 dolos 0.34.0"
  exit 1
fi

PACKAGE_NAME="$1"
NEW_VERSION="$2"
PACKAGE_DIR="packages/${PACKAGE_NAME}"

if [[ ! -d "$PACKAGE_DIR" ]]; then
  echo "ERROR: Package directory not found: $PACKAGE_DIR"
  exit 1
fi

# Find the latest version file by sorting versions
LATEST_FILE=$(find "$PACKAGE_DIR" -maxdepth 1 -name "${PACKAGE_NAME}-*.yaml" -type f | sort -V | tail -1)

if [[ -z "$LATEST_FILE" ]]; then
  echo "ERROR: No existing version files found for package: $PACKAGE_NAME"
  exit 1
fi

# Extract old version from the latest file
OLD_VERSION=$(grep -E '^version:' "$LATEST_FILE" | head -1 | awk '{print $2}')
NEW_FILE="${PACKAGE_DIR}/${PACKAGE_NAME}-${NEW_VERSION}.yaml"

if [[ -f "$NEW_FILE" ]]; then
  echo "ERROR: Version file already exists: $NEW_FILE"
  exit 1
fi

echo "Creating new version for $PACKAGE_NAME"
echo "  Source: $LATEST_FILE (version $OLD_VERSION)"
echo "  Target: $NEW_FILE (version $NEW_VERSION)"

# Copy the file
cp "$LATEST_FILE" "$NEW_FILE"

# Update the version field
sed -i "s/^version: .*/version: ${NEW_VERSION}/" "$NEW_FILE"

# Update Docker image tags
# Handle various tag formats:
# - ghcr.io/blinklabs-io/dingo:0.20.0 -> ghcr.io/blinklabs-io/dingo:0.21.0
# - ghcr.io/txpipe/dolos:v0.32.0 -> ghcr.io/txpipe/dolos:v0.34.0
# - cardanosolutions/ogmios:v6.14.0 -> cardanosolutions/ogmios:v6.15.0

# Replace version with v prefix (e.g., :v0.32.0 -> :v0.34.0)
sed -i "s/:v${OLD_VERSION}/:v${NEW_VERSION}/g" "$NEW_FILE"

# Replace version without v prefix (e.g., :0.20.0 -> :0.21.0)
# Only match if not preceded by 'v' to avoid double replacement
sed -i "s/:\([^v]\)${OLD_VERSION}/:\1${NEW_VERSION}/g" "$NEW_FILE"
sed -i "s/:${OLD_VERSION}\([^0-9]\)/:${NEW_VERSION}\1/g" "$NEW_FILE"
sed -i "s/:${OLD_VERSION}$/:${NEW_VERSION}/g" "$NEW_FILE"

# Handle cardano-config special case (version-1 suffix in image tag)
# e.g., :20251128-1 -> :20260115-1
if [[ "$PACKAGE_NAME" == "cardano-config" ]]; then
  sed -i "s/:${OLD_VERSION}-1/:${NEW_VERSION}-1/g" "$NEW_FILE"
fi

echo ""
echo "Created: $NEW_FILE"
echo ""
echo "Please review the changes:"
diff -u "$LATEST_FILE" "$NEW_FILE" || true
