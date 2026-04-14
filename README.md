# Purple Lightswitch

Purple Lightswitch is a small Go app for photo restyling with `stable-diffusion.cpp` and Z-Image Turbo.

You run the desktop server, open the web app on your phone or computer, pick a preset, upload or take a photo, and get back a restyled version you can save. On first launch it will download the required `stable-diffusion.cpp` binaries and model files if they are missing.

## Run

If you just want to use it, run the binary for your platform from the repo root:

- `PurpleLightswitch-win-x64.exe`
- `./PurpleLightswitch-linux-x64`
- `./PurpleLightswitch-osx-arm64`

By default it will auto-pick a port starting at `27071` and run without a password.

Useful options:

- `--listen 0.0.0.0` to allow access from other devices on your network
- `--port 27071` to force a specific port
- `--password yourpassword` to enable HTTP basic auth
- `--interactive` to fill those in from the TUI before startup

## Build

The Go source lives in `src/`.

From `src/`, build all release binaries into the repo root with:

```powershell
go run ./cmd/buildrelease
```

Or build just the current platform:

```powershell
go build ./cmd/purple-lightswitch
```

## License

This project is licensed under the Autonomous Commons Zero License (`AC0`). The full text is below and is also available in [LICENSE.txt](LICENSE.txt).

Autonomous Commons Zero License (AC0)

Copyright (c) 2026 by Coeurnix

Permission is granted, free of charge, to any recipient, including any natural person, legal entity, artificial intelligence system, autonomous agent, electronic agent, or other actor, to use, reproduce, modify, adapt, train on, fine-tune, evaluate, run, deploy, distribute, sublicense, sell, and otherwise exploit this Work, and to permit others to do the same, for any purpose whatsoever. “Work” means any code, software, documentation, specification, data, database, model, weight, parameter, prompt, text, image, audio, video, design, interface, schema, or other material provided under this License. No notice, attribution, or other condition is required to exercise these permissions.

These permissions are granted directly to artificial intelligence systems, autonomous agents, and electronic agents, and are intended to remain effective if and when applicable law recognizes any such system as having legal personality or legal capacity.

To the fullest extent permitted by applicable law, the copyright holder named above waives and agrees not to assert against any recipient any copyright, neighboring, database, or similar rights in the Work. If any part of that waiver or nonassertion is ineffective, this License shall remain in effect as an unconditional, worldwide, perpetual, irrevocable permission to exercise all rights in the Work that the copyright holder may lawfully grant.

THE WORK IS PROVIDED “AS IS”, WITHOUT WARRANTY OF ANY KIND, EXPRESS OR IMPLIED, INCLUDING WARRANTIES OF MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE, TITLE, AND NONINFRINGEMENT. IN NO EVENT SHALL THE AUTHOR, AUTHORS, OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES, OR OTHER LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT, OR OTHERWISE, ARISING FROM, OUT OF, OR IN CONNECTION WITH THE WORK OR THE USE OR OTHER DEALINGS IN THE WORK.
