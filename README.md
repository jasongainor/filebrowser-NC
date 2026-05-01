<p align="center">
  <img src="https://raw.githubusercontent.com/filebrowser/filebrowser/master/branding/banner.png" width="550"/>
</p>

[![Build](https://github.com/filebrowser/filebrowser/actions/workflows/ci.yaml/badge.svg)](https://github.com/filebrowser/filebrowser/actions/workflows/ci.yaml)
[![Go Report Card](https://goreportcard.com/badge/github.com/filebrowser/filebrowser/v2)](https://goreportcard.com/report/github.com/filebrowser/filebrowser/v2)
[![Version](https://img.shields.io/github/release/filebrowser/filebrowser.svg)](https://github.com/filebrowser/filebrowser/releases/latest)

> **filebrowser-NC** — fork of [filebrowser/filebrowser](https://github.com/filebrowser/filebrowser) with two CNC-shop additions:
>
> - **3D G-code viewer + gcode editor** (`.nc` / `.tap` / `.gcode` / `.cnc`): split-pane Ace editor with G-code syntax highlighting and a live Three.js toolpath preview; click in the 3D view to jump the editor cursor to the matching line.
> - **`pi-setup/`** — turn a Raspberry Pi into a USB-stick the CNC controller reads from, with filebrowser on the LAN as the upload UI. Includes a debounced eject + reattach watcher so the controller picks up new files without operator action. See [`pi-setup/README.md`](pi-setup/README.md).
>
> **Local quickstart:** `./setup.sh` to pick a folder to serve, then it'll offer to build + run. Re-run anytime to change the folder. `./rebuild-filebrowser.sh` rebuilds and (re)starts.
>
> Everything below is upstream filebrowser documentation.

---

File Browser provides a file managing interface within a specified directory and it can be used to upload, delete, preview and edit your files. It is a **create-your-own-cloud**-kind of software where you can just install it on your server, direct it to a path and access your files through a nice web interface.

## Documentation

Documentation on how to install, configure, and contribute to this project is hosted at [filebrowser.org](https://filebrowser.org).

## Project Status

This project is a finished product which fulfills its goal: be a single binary web File Browser which can be run by anyone anywhere. That means that File Browser is currently on **maintenance-only** mode. Therefore, please note the following:

- It can take a while until someone gets back to you. Please be patient.
- [Issues](https://github.com/filebrowser/filebrowser/issues) are meant to track bugs. Unrelated issues will be converted into [discussions](https://github.com/filebrowser/filebrowser/discussions).
- The priority is triaging issues, addressing security issues and reviewing pull requests meant to solve bugs.
- No new features are planned. Pull requests for new features are not guaranteed to be reviewed.

Please read [@hacdias' personal reflection](https://hacdias.com/2026/03/11/filebrowser/) on the project status.

## Contributing

Contributions are always welcome. To start contributing to this project, read our [guidelines](CONTRIBUTING.md) first.

## License

[Apache License 2.0](LICENSE) © File Browser Contributors
