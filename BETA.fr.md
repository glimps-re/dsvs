# La beta de dsvs

[English](BETA.md) · [README](README.fr.md)

La beta donne un avant-goût de la prochaine version de dsvs. Son binaire, `dsvs-beta`, s'installe **à côté** de `dsvs` et reste chez lui : ses propres sockets, son unité, son service, son répartiteur et ses projets retenus, et sa propre icône, aux barres colorées là où celles du stable sont blanches.

Il lit sa propre configuration, `config.beta.dsvs.ts` ou `dsvs.beta.json`, cherchée en remontant depuis le dossier courant, et **ne se rabat jamais** sur `config.dsvs.ts` : un projet tourne sur la beta une fois qu'on a copié sa configuration, ou qu'on nomme un fichier avec `--config`. Donnez un autre port à la copie, et les deux binaires servent le projet en même temps. Le stable reste votre filet : quoi que fasse la beta, `dsvs` continue de faire tourner vos projets comme avant.

Ce que la beta ajoute, et toute sa documentation, vivent sur la branche `beta` : [BETA.fr.md](https://github.com/glimps-re/dsvs/blob/beta/BETA.fr.md).

## L'installer

- Depuis le panneau de `dsvs` : **install the beta** apparaît dès qu'une beta est publiée et qu'aucune n'est installée. Une seconde icône apparaît.
- Depuis un terminal, même sans stable :

  ```sh
  curl -fsSL https://github.com/glimps-re/dsvs/releases/latest/download/install.sh | DSVS_CHANNEL=beta sh
  ```

`dsvs-beta` suit les pré-versions (`vX.Y.Z-beta.N`), et une version stable qui dépasse la dernière beta.

## La retirer

**uninstall the beta**, dans le panneau de `dsvs-beta`, ferme ses sessions, retire son unité et son binaire, et laisse `dsvs` tel qu'il était.
