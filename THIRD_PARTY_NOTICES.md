# Third-party notices

kownload contains modified code from the following projects. Original copyright notices, license texts and Git history are retained.

| Project | Use | License |
| --- | --- | --- |
| [GopeedLab/gopeed](https://github.com/GopeedLab/gopeed), v1.9.3 | Download core and Flutter application foundation, modified for kownload | GPL-3.0, see [LICENSE](LICENSE) |
| [LinkSwift](https://github.com/hmjz100/LinkSwift) | Cloud-drive file selection reference | AGPL-3.0, see [notice](third_party/linkswift/NOTICE.md) and [license](third_party/linkswift/LICENSE) |
| [OpenList](https://github.com/OpenListTeam/OpenList) | Cloud-drive adapter reference | AGPL-3.0, see [notice](third_party/openlist/NOTICE.md) and [license](third_party/openlist/LICENSE) |
| [yt-dlp](https://github.com/yt-dlp/yt-dlp), [FFmpeg](https://ffmpeg.org), Node.js | External media tools invoked at runtime | Their respective licenses |

Compatibility identifiers, including `com.gopeed.gopeed`, `gopeed:` and internal module/package names, remain unchanged so existing data and browser integration continue to work. These are technical identifiers; the application is displayed as kownload.

Source code corresponding to each released binary is available from the release tag in this repository. Original project documentation is retained in [docs/GOPEED_UPSTREAM.md](docs/GOPEED_UPSTREAM.md).

## Distribution materials

macOS packages include `Contents/Resources/legal/`: exact linked Go module
copyright/license files and source archive locations, Go/Flutter/Dart SDK
licenses, native CocoaPods acknowledgements, and the notices for the embedded
npm polyfills. Flutter's generated `NOTICES.Z` remains in its asset bundle.
Browser helper ZIPs carry LICENSE, this notice, third_party licenses and SOURCE.txt.

Corresponding Source is supplied as a versioned source archive alongside each
binary release, including the selected Git commit, build scripts and linked Go
module source archives. Exact dependency versions and source locations are in
the lockfiles and generated module manifest. See [release procedure](docs/RELEASING.md).
Local unpublished candidates explicitly identify their unpublished state.
