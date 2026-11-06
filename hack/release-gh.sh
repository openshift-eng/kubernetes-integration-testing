#!/bin/bash
set -eu

IMAGE="${IMAGE:-quay.io/openshift-eng-kit/kit:latest}"
RELEASE_TAG="${RELEASE_TAG:-latest}"
PLATFORMS="${PLATFORMS:-linux/amd64 linux/arm64 darwin/amd64 darwin/arm64}"
DRY_RUN="${DRY_RUN:-false}"
REPO="${GITHUB_REPOSITORY:-openshift-eng/kubernetes-integration-testing}"

for cmd in skopeo jq gh go git; do
    if ! command -v "$cmd" >/dev/null 2>&1; then
        echo "error: $cmd is required but not found" >&2
        exit 1
    fi
done

echo "Inspecting image ${IMAGE}..."
INSPECT=$(skopeo inspect "docker://${IMAGE}")

COMMIT_SHA=$(echo "$INSPECT" | jq -r '.Labels["io.openshift.build.commit.id"]')
if [ -z "$COMMIT_SHA" ] || [ "$COMMIT_SHA" = "null" ]; then
    echo "error: failed to extract commit SHA from image labels" >&2
    exit 1
fi

DIGEST=$(echo "$INSPECT" | jq -r '.Digest')
IMAGE_REF="$(echo "$INSPECT" | jq -r '.Name')@${DIGEST}"
echo "Image digest: ${DIGEST}"
echo "Image ref: ${IMAGE_REF}"
echo "Image commit: ${COMMIT_SHA}"

CURRENT_DIGEST=$(gh release view "$RELEASE_TAG" --repo "$REPO" --json body --jq '.body' 2>/dev/null \
    | grep -oP '(?<=Digest: `)[^`]+' || true)
if [ "$CURRENT_DIGEST" = "$DIGEST" ]; then
    echo "Release is already up to date, nothing to do."
    exit 0
fi

WORKTREE=$(mktemp -d)
cleanup() { git worktree remove --force "$WORKTREE" 2>/dev/null || true; }
trap cleanup EXIT

echo "Creating worktree at ${WORKTREE} for commit ${COMMIT_SHA}..."
git worktree add --detach "$WORKTREE" "$COMMIT_SHA"

echo "Running go-deps..."
pushd "$WORKTREE" >/dev/null
make go-deps
popd >/dev/null

echo "Building kit for: ${PLATFORMS}"
for platform in $PLATFORMS; do
    os="${platform%/*}"
    arch="${platform#*/}"
    output="${WORKTREE}/bin/kit-${os}-${arch}"
    echo "  ${platform} -> kit-${os}-${arch}"
    pushd "$WORKTREE" >/dev/null
    TARGETS=kit DEFAULT_ARCH="$arch" GOOS="$os" GOARCH="$arch" \
        DEFAULT_CONTROLLER_IMAGE="$IMAGE_REF" \
        hack/build.sh
    mv bin/kit "$output"
    popd >/dev/null
done

if [ "$DRY_RUN" = "true" ]; then
    echo "Dry run: skipping release creation"
    ls -lh "${WORKTREE}"/bin/kit-*
    exit 0
fi

echo "Updating release ${RELEASE_TAG}..."
gh release delete "$RELEASE_TAG" --repo "$REPO" --yes --cleanup-tag 2>/dev/null || true

gh release create "$RELEASE_TAG" \
    --repo "$REPO" \
    --target "$COMMIT_SHA" \
    --title "Latest" \
    --notes "Automated release tracking \`${IMAGE}\`.

**Digest:** \`${DIGEST}\`
**Commit:** \`${COMMIT_SHA}\`" \
    "${WORKTREE}"/bin/kit-*

echo "Release ${RELEASE_TAG} created."
