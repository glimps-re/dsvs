# dsvs beta

[Français](BETA.fr.md) · [README](README.md)

The beta previews the next version of dsvs. Its binary, `dsvs-beta`, installs **beside** `dsvs` and keeps to itself: its own sockets, unit, service, hub and remembered projects, and its own icon, with colored bars where the stable's are white.

It reads its own configuration, `config.beta.dsvs.ts` or `dsvs.beta.json`, searched up from the current directory, and **never falls back** to `config.dsvs.ts`: a project runs on the beta once you copy its configuration, or name a file with `--config`. Give the copy another port and both binaries serve the project at once. The stable stays your fallback: whatever the beta does, `dsvs` keeps running your projects as before.

What the beta adds, and its whole documentation, live on the `beta` branch: [BETA.md](https://github.com/glimps-re/dsvs/blob/beta/BETA.md).

## Install it

- From the panel of `dsvs`: **install the beta** shows up once a beta is published and none is installed. A second icon appears.
- From a terminal, even without a stable:

  ```sh
  curl -fsSL https://github.com/glimps-re/dsvs/releases/latest/download/install.sh | DSVS_CHANNEL=beta sh
  ```

`dsvs-beta` follows the pre-releases (`vX.Y.Z-beta.N`), and a stable release that overtakes the last beta.

## Remove it

**uninstall the beta**, in the panel of `dsvs-beta`, closes its sessions, removes its unit and its binary, and leaves `dsvs` as it was.
