# grab

A download manager for Mac, in the spirit of Internet Download Manager: files are fetched in **chunks downloaded in parallel** from the server, a progress bar lives in your terminal, **Ctrl+C pauses**, and a relaunch **resumes exactly where it stopped** — even after a network cut. It also handles segmented video streams (HLS `m3u8` playlists), stitches them into a single `.ts` file, and remuxes it to `.mp4` at the end when ffmpeg is present.

![Language](https://img.shields.io/badge/language-Go-00ADD8?logo=go&logoColor=white) ![Platform](https://img.shields.io/badge/macOS-Apple_silicon-000000?logo=apple&logoColor=white) ![Interface](https://img.shields.io/badge/terminal-progress_bar-4EAA25) ![Browser](https://img.shields.io/badge/extension-Chrome_MV3-4285F4?logo=googlechrome&logoColor=white) ![Video](https://img.shields.io/badge/remux-ffmpeg-007808?logo=ffmpeg&logoColor=white) ![Dependencies](https://img.shields.io/badge/go.mod-zero_dependencies-2496ED)

The badge band above reads, left to right: written in **Go** (standard library only, zero external dependencies), runs on **macOS** (Apple silicon), lives in the **terminal**, is also driven from **Chrome** through a Manifest V3 extension, remuxes to **MP4** with **ffmpeg**, and relies on nothing else.

## Table of contents

- [A taste](#a-taste)
- [Install](#install)
- [Commands](#commands)
- [Pause and resume](#pause-and-resume)
- [Segmented video streams (m3u8)](#segmented-video-streams-m3u8)
- [The browser extension](#the-browser-extension)
- [Where things live on disk](#where-things-live-on-disk)
- [What grab does not do yet](#what-grab-does-not-do-yet)

## A taste

```
grab https://example.com/big-video.mp4
→ big-video.mp4  running   ████████░░░░░░  46%  138/300 MB  · 12 MB/s · left 0:13
^C
Paused by Ctrl+C — everything already written is kept.
Run `grab go` to resume.
```

## Install

You need [Go](https://go.dev) on the machine, then:

```
cd ~/DEV/grab && go build -o grab ./cmd/grab
sudo cp grab /usr/local/bin/        # optional: lets you type `grab` from anywhere
```

For the `.mp4` copy of `.ts` streams: `brew install ffmpeg`. Without it, grab keeps the `.ts`, which plays fine in IINA or VLC.

## Commands

```
grab <url>…         download right away (Ctrl+C: pause, relaunch to resume)
grab add <url>…     queue without starting
grab go             start (and resume) whatever is waiting
grab ls             show the queue and recent downloads
grab history        show finished and failed downloads
grab pause <id>     pause a running (or not-yet-started) download
grab resume [id…]   resume paused downloads (all of them if no id)
grab rm <id>        abandon an incomplete download
grab clear          empty the history
grab serve [port]   local receiver for the browser extension + continuous queue (default 48113)
grab help           this help
```

Flags go anywhere: `-d <dir>` (where files land, default `~/Downloads`), `-s <n>` (parallel chunks per file, 1 to 32, default 8). Every download carries an 8-character id, shown by `grab ls` and `grab history`; for `pause`, `resume` and `rm`, an unambiguous prefix is enough.

## Pause and resume

`Ctrl+C` (or `grab pause <id>`) stops grab cleanly and keeps the incomplete file next to the final name, suffixed `.grabpart`. A power cut or a crash behaves the same on the next relaunch. Every chunk saves its progress to disk every 250 ms: at worst you lose half a second of downloading, never more. Resuming asks the server for only the missing bytes (`Range: bytes=…`) and finishes the file as if nothing happened.

If the server does not support byte ranges, grab falls back to a plain single-connection download (from scratch after every pause) and tells you in clear language; the file stays correct, only the resume loses its advantage.

## Segmented video streams (m3u8)

Many web players serve video through HLS playlists rather than one big file. grab spots the playlist — even disguised (a `.m3u8` address, a `mpegurl` content type, or a `#EXTM3U` signature at the head of the file) — picks the variant with the highest advertised bandwidth when the master lists several, downloads each segment in order, and stitches them together. Pause and resume work here too, at the segment level.

The final file takes the segments' extension, usually `.ts`; when ffmpeg is present, grab copies it into a `.mp4` without re-encoding (zero loss) and removes the intermediate `.ts`. Without ffmpeg, the `.ts` stays put and plays in IINA or VLC.

## The browser extension

A "⤓ grab" button sits above every direct video player on a page; clicking it sends the video to the queue. A right click on any file link offers "Download with grab". The local receiver is guarded two ways: it only listens on `127.0.0.1`, and every request must carry the secret token stored in `~/.grab/serve-token` (mode 600) — a malicious web page cannot push anything through.

```
1. run `grab serve` in a terminal; note the printed token
2. Chrome → chrome://extensions → developer mode → "Load unpacked"
   → pick the ~/DEV/grab/extension folder
3. click the grab icon → paste the token → Save, then Test
```

While `grab serve` runs, the queue keeps flowing: as soon as a download finishes, the next one starts. `Ctrl+C` pauses everything cleanly.

## Where things live on disk

| Path | Contents |
| --- | --- |
| `~/DEV/grab` | the code: the engine (Go 1.22+), the queue, the display, the tests |
| `~/DEV/grab/extension/` | the Chrome extension: manifest, page script, background worker, popup |
| `~/.grab/state.json` | state: queue, chunk-by-chunk progress, history |
| `~/.grab/serve-token` | the token shared with the extension |
| `destination/file.grabpart` | the incomplete download, deleted once the file is complete |

## What grab does not do yet

- players that keep the video in memory behind a `blob:` — the button shows up greyed out, marked "not supported yet", rather than lying;
- AES-encrypted streams — a clean refusal instead of a blind download;
- YouTube, Vimeo and friends — closed platforms with no direct file in the page; touching them means reading and accepting their terms of use, a decision to make consciously later;
- Safari — the extension is written for Manifest V3; porting it later is adaptation, not a rewrite.

## In short

One executable, a browser extension, a local receiver and ffmpeg — nothing else. Version 0.2.0.
