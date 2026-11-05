#!/usr/bin/env bash
# Build Windows x64 distribution from a macOS or Linux checkout.
set -euo pipefail
root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
out=${1:-"$root/releases"}
mkdir -p -- "$out"
out=$(cd -- "$out" && pwd)
scratch=$(mktemp -d "${TMPDIR:-/tmp}/uulink-windows.XXXXXX")
trap 'rm -rf -- "$scratch"' EXIT
stage="$scratch/stage"
src="$scratch/src"
dist=UU-Link-0.3.5-windows-x64
mkdir -p -- "$stage" "$src"
cp -- "$root"/*.go "$root/go.mod" "$root/go.sum" "$src/"
cp -R -- "$root/gui_assets" "$src/"
mkdir -p -- "$src/native-ui/windows"
cp -R -- "$root/native-ui/windows/setup-assets" "$src/native-ui/windows/"
cp -- "$root/native-ui/windows/AppIcon.ico" "$src/native-ui/windows/"
mkdir -p -- "$src/assets"
cp -- "$root/assets/no-desktop.png" "$root/assets/no-desktop.h264" "$src/assets/"
cp -- "$root/native-ui/windows/AppIcon.ico" "$scratch/AppIcon.ico"
(
  cd -- "$src"
  go run github.com/tc-hib/go-winres@v0.3.3 simply --arch amd64 --out "$src/rsrc" \
    --icon "$scratch/AppIcon.ico" --manifest gui --product-version 0.3.5.1 --file-version 0.3.5.1 \
    --product-name "UU Link" --file-description "UU Link" --original-filename "UU Link.exe"
  CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -buildvcs=false -ldflags='-s -w -H=windowsgui' -o "$stage/UU Link.exe" .
)
(
  cd -- "$root/experiments/udp-game-adaptive"
  CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -buildvcs=false -ldflags='-s -w' -o "$stage/uulink-engine.exe" .
)
cp -- "$root/THIRD_PARTY_NOTICES.txt" "$stage/"
cp -- "$root/native-ui/windows/quickstart.txt" "$stage/README.txt"
# Microsoft Evergreen Bootstrapper, obtained through the official WebView2 distribution link.
bootstrapper="$scratch/MicrosoftEdgeWebview2Setup.exe"
curl --proto '=https' --proto-redir '=https' --tlsv1.2 --fail --location --silent --show-error --max-time 60 \
  'https://msedge.sf.dl.delivery.mp.microsoft.com/filestreamingservice/files/068be74d-a831-494f-8f57-c173756d5a33/MicrosoftEdgeWebview2Setup.exe' \
  -o "$bootstrapper"
printf '%s  %s\n' cf2f2445fef0c5c14e372045f84998771eeb8bb01d59be4afd00692fa160082e "$bootstrapper" | shasum -a 256 -c
makensis -INPUTCHARSET UTF8 -V3 "-DSTAGE=$stage" "-DOUTPUT=$scratch/install-core.exe" "-DAPP_ICON=$scratch/AppIcon.ico" \
  "-DBOOTSTRAPPER=$bootstrapper" "$root/native-ui/windows/installer.nsi"
cp -- "$scratch/install-core.exe" "$src/assets/windows-install-core.exe"
cp -- "$bootstrapper" "$src/assets/webview2-setup.exe"
(
  cd -- "$src"
  go run github.com/tc-hib/go-winres@v0.3.3 simply --arch amd64 --out "$src/rsrc" \
    --icon "$scratch/AppIcon.ico" --manifest gui --product-version 0.3.5.1 --file-version 0.3.5.1 \
    --product-name "UU Link" --file-description "UU Link Setup" --original-filename "UU Link Setup.exe"
  CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -tags uulink_setup -trimpath -buildvcs=false -ldflags='-s -w -H=windowsgui' -o "$scratch/$dist-setup.exe" .
)
(
  cd -- "$stage"
  zip -X -9 "$scratch/$dist-portable.zip" "UU Link.exe" uulink-engine.exe README.txt THIRD_PARTY_NOTICES.txt
)
(
  cd -- "$scratch"
  shasum -a 256 "$dist-setup.exe" "$dist-portable.zip" > "$dist-SHA256SUMS"
)
mv -- "$scratch/$dist-setup.exe" "$scratch/$dist-portable.zip" "$scratch/$dist-SHA256SUMS" "$out/"
printf 'Windows x64 preview: %s/%s-*\n' "$out" "$dist"
