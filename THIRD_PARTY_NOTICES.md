# Third-Party Notices

This repository is primarily distributed under the GPL-2.0 License in `LICENSE`.
That license applies to Temporal-authored source code, documentation, or website
content files unless a file-level notice or this document says otherwise.

Third-party components may be governed by different license terms. Redistributors
are responsible for preserving the applicable notices and source availability
obligations for those components.

## doom1.wad

The file `wad/doom1.wad`, when present, is DOOM shareware game data from id Software.
It is game data, not source code, and it is not covered by this repository's GPL.

The DOOM shareware data is freely redistributable, but it remains proprietary
and non-open-source game data with terms separate from the source code in this
repository. Credit for DOOM and the shareware game data belongs to id Software.

A DOOM WAD-specific notice is included at `licenses/DOOM1-WAD.txt`.

## gore

The `third_party/gore` directory is a git submodule of
[gore](https://github.com/AndreRenaud/gore), a pure Go port of the DOOM engine
by Andre Renaud and contributors. gore is licensed under the GNU General Public
License, version 2. Its license is at `third_party/gore/LICENSE`. gore is
compiled into `doom.wasm`, the engine that runs in both the Temporal worker and
the browser.

Credit for the DOOM engine belongs to id Software, and to the authors of the
projects above.
