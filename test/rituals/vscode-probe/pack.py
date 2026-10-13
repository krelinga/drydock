#!/usr/bin/env python3
"""Packs the ritual probe into a .vsix with nothing but Python's standard library.

    python3 test/rituals/vscode-probe/pack.py [out.vsix]

A .vsix is a zip: the extension under extension/, a manifest, and a content
types file. This writes exactly the probe's package.json and extension.js,
so what is installed is what test/rituals/vscode-connect.md shows, and
prints the file's SHA-256 for the run's record. The archive is reproducible:
fixed entry order, timestamps (1980-01-01) and attributes, so its digest is a
function of the two files alone, and anyone can re-pack a commit and compare.
Install it only into a throwaway workspace container (the ritual's part 2),
never on the laptop.
"""

import hashlib
import json
import os
import sys
import zipfile
from xml.sax.saxutils import escape

HERE = os.path.dirname(os.path.abspath(__file__))
FILES = ("package.json", "extension.js")
EPOCH = (1980, 1, 1, 0, 0, 0)


def entry(z: zipfile.ZipFile, name: str, data: bytes) -> None:
    """Writes one entry with nothing in it but its name and bytes."""
    info = zipfile.ZipInfo(name, date_time=EPOCH)
    info.compress_type = zipfile.ZIP_STORED  # no deflate: zlib versions differ
    info.create_system = 3  # Unix, whatever packs it
    info.external_attr = 0o100644 << 16  # a regular file, rw-r--r--
    z.writestr(info, data)


def main() -> None:
    out = sys.argv[1] if len(sys.argv) > 1 else "drydock-ritual-probe.vsix"
    with open(os.path.join(HERE, "package.json"), encoding="utf-8") as f:
        pkg = json.load(f)
    x = {k: escape(str(v), {'"': "&quot;"}) for k, v in pkg.items() if isinstance(v, str)}
    engine = escape(pkg["engines"]["vscode"], {'"': "&quot;"})
    manifest = f"""<?xml version="1.0" encoding="utf-8"?>
<PackageManifest Version="2.0.0" xmlns="http://schemas.microsoft.com/developer/vsx-schema/2011" xmlns:d="http://schemas.microsoft.com/developer/vsx-schema-design/2011">
  <Metadata>
    <Identity Language="en-US" Id="{x['name']}" Version="{x['version']}" Publisher="{x['publisher']}" />
    <DisplayName>{x['displayName']}</DisplayName>
    <Description xml:space="preserve">{x['description']}</Description>
    <Properties>
      <Property Id="Microsoft.VisualStudio.Code.Engine" Value="{engine}" />
      <Property Id="Microsoft.VisualStudio.Code.ExtensionKind" Value="workspace" />
    </Properties>
  </Metadata>
  <Installation>
    <InstallationTarget Id="Microsoft.VisualStudio.Code" />
  </Installation>
  <Dependencies />
  <Assets>
    <Asset Type="Microsoft.VisualStudio.Code.Manifest" Path="extension/package.json" Addressable="true" />
  </Assets>
</PackageManifest>
"""
    types = """<?xml version="1.0" encoding="utf-8"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension=".json" ContentType="application/json" /><Default Extension=".js" ContentType="application/javascript" /><Default Extension=".vsixmanifest" ContentType="text/xml" /></Types>
"""
    entries = {"[Content_Types].xml": types.encode(), "extension.vsixmanifest": manifest.encode()}
    for name in FILES:
        with open(os.path.join(HERE, name), "rb") as f:
            entries[f"extension/{name}"] = f.read()
    with zipfile.ZipFile(out, "w") as z:
        for name in sorted(entries):
            entry(z, name, entries[name])
    with open(out, "rb") as f:
        digest = hashlib.sha256(f.read()).hexdigest()
    print(f"{out}  sha256 {digest}")


if __name__ == "__main__":
    main()
