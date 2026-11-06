# dsvs — un panneau de contrôle pour les serveurs de développement

[English](README.md)

dsvs fait tourner les apps d'un projet — `ng serve`, `vite dev`, une API, n'importe quel processus — et les sert **derrière un port unique**, comme un ingress en production : le `Host` de la requête choisit la façade, et son chemin choisit l'app, la cible de proxy ou la redirection qui est derrière. Une ligne de commande, une TUI et un panneau ouvert depuis une icône de la barre d'état les pilotent ; chaque app a son journal ; rien ne démarre tout seul, on nomme ce qu'on veut.

Plusieurs projets, ou plusieurs worktrees du même, peuvent tourner **en même temps sur le même port** : un répartiteur tient le port pour tous, et donne à chaque session les connexions des hôtes qu'elle déclare. Les apps, elles, n'ouvrent aucun port sur la machine : elles écoutent sur des adresses virtuelles, dans un namespace réseau privé qui ne demande aucun privilège.

- Une **app** est un processus que la configuration déclare.
- Une **façade** est un hôte et sa table de routage : elle sert des apps, des cibles de proxy et des redirections, et ne démarre rien elle-même.
- Un **groupe** est un ensemble nommé d'apps, qu'on démarre et qu'on lit d'un seul geste.
- Une **session** fait tourner les apps d'une configuration. Elle vit sans terminal et survit aux commandes et aux interfaces qui la regardent.
- Le **service** tourne en arrière-plan : il pose l'icône dans la barre d'état et ouvre le panneau.
- Le **répartiteur** tient le port. Il n'appartient à aucun service : la première session qui a besoin d'un port le lance, et il s'en va avec la dernière.

Tout démarre au besoin, et le dit.

## Prérequis

- **Linux, avec une session `systemd --user`.** Le réseau privé est fait de namespaces réseau, et c'est systemd qui lance le service et met chaque session dans une boîte. Sans lui, dsvs tourne en mode dégradé, et le dit.
- **Node 22.18 ou plus (23.6 ou plus dans la branche 23)** pour un `config.dsvs.ts` : dsvs importe le TypeScript tel quel, grâce au type stripping que node active par défaut depuis ces versions. Un `dsvs.json` ne demande pas node.
- **`curl`** pour installer, et **`~/.local/bin` dans le `PATH`**.
- **Une barre d'état X11 qui accueille les icônes XEmbed**, pour l'icône. Sans elle, tout se fait depuis le terminal et la TUI, et `dsvs panel` ouvre le panneau sur un affichage X.

## Installer

```sh
curl -fsSL https://github.com/glimps-re/dsvs/releases/latest/download/install.sh | sh
dsvs --version
```

Le script télécharge le binaire de la dernière release pour cette machine (amd64 ou arm64), le vérifie contre le `checksums.txt` de la release avant qu'il ne prenne aucune place, et le pose dans `~/.local/bin`. Il écrit ensuite une unité `systemd --user` et démarre le service : l'icône est dans la barre d'état quand le prompt revient, et la dernière ligne dit ce qui s'est passé. L'unité relance le service à chaque ouverture de session. Comme c'est systemd qui le lance, et non un shell, le service prend l'environnement de votre shell de login : ce qu'il lance trouve `node`, `pnpm` et le reste comme un terminal le ferait.

- `DSVS_VERSION=v0.1.0` installe cette version au lieu de la dernière.
- `DSVS_DIR=/un/bin` installe dans un autre dossier.
- `DSVS_RELEASES` désigne un miroir organisé comme les releases GitHub.

`dsvs --version` dit ce qui est installé, `dsvs status` ce qui tourne. [CONTRIBUTING.md](CONTRIBUTING.md#releases) montre comment vérifier une release à la main, empreintes et provenance du build.

### La beta

La beta donne un avant-goût de la prochaine version. Son binaire, `dsvs-beta`, s'installe **à côté** de `dsvs` et reste chez lui : ses propres sockets, son unité, son service, son répartiteur et ses projets retenus, et sa propre icône, aux barres ambre là où celles du stable sont blanches. Il lit sa propre configuration, `config.beta.dsvs.ts` ou `dsvs.beta.json`, cherchée en remontant depuis le dossier courant, et **ne se rabat jamais** sur `config.dsvs.ts` : un projet tourne sur la beta une fois qu'on a copié sa configuration, ou qu'on nomme un fichier avec `--config`. Donnez un autre port à la copie, et les deux binaires servent le projet en même temps. Le stable reste votre filet.

- Depuis le panneau de `dsvs` : **install the beta** apparaît dès qu'une beta est publiée et qu'aucune n'est installée. Une seconde icône apparaît.
- Depuis un terminal, même sans stable :

  ```sh
  curl -fsSL https://github.com/glimps-re/dsvs/releases/latest/download/install.sh | DSVS_CHANNEL=beta sh
  ```

`dsvs-beta` suit les pré-versions (`vX.Y.Z-beta.N`), et une version stable qui dépasse la dernière beta. **uninstall the beta**, dans son propre panneau, ferme ses sessions, retire son unité et son binaire, et laisse `dsvs` tel qu'il était.

## Mettre à jour

`dsvs update`, ou **check for updates** puis **update to vX.Y.Z** dans le panneau, qui cherche une version plus récente à chaque ouverture. Un binaire installé depuis une release télécharge le nouveau en HTTPS et le vérifie contre les empreintes de sa release avant de remplacer quoi que ce soit : `dsvs` suit les versions stables, `dsvs-beta` les pré-versions aussi. L'ancien binaire reste à côté du nouveau, en `.old`, et reprend sa place si le nouveau ne démarre pas.

Une mise à jour n'arrête pas vos apps. Chaque session passe la main au nouveau binaire sans changer de PID : sa boîte systemd ne voit rien passer, et ses apps restent ses enfants. La mise à jour en rend compte session par session, en demandant à chacune la version qu'elle porte désormais. Le service et le portier du réseau privé passent la main de la même façon. Le répartiteur n'est pas dérangé : il prend le nouveau binaire en s'en allant avec sa dernière session, et une session qui le perd entre-temps en relance un sans rien lâcher.

Une session qui tourne hors de la boîte systemd garde son binaire jusqu'à ce qu'elle s'arrête : là, le signal de mort du parent est le seul filet contre les apps orphelines, et il ne survivrait pas à la passation. Une version qui change le protocole entre les sessions et leur répartiteur — c'est rare, et le journal le dit — refuse de se mélanger : tant qu'une session de l'ancienne tourne, une nouvelle session s'arrête sur un message qui nomme les deux versions. Fermer les sessions ouvertes suffit.

## Construire depuis les sources

Avec Go et un clone, le chemin de qui travaille sur dsvs :

```sh
git clone https://github.com/glimps-re/dsvs.git
cd dsvs && ./build.sh --install     # → ~/.local/bin/dsvs, version = git describe
```

Le binaire prend le nom de la branche — `main` donne `dsvs`, `feat/x` donne `dsvs-x` — et tout ce qui l'isole en dérive : sockets, icône, répartiteur, unité systemd et projets retenus. Un binaire de branche tourne donc à côté de votre `dsvs` de tous les jours sans le toucher, et son icône a des barres ambre. Le port, lui, est global : le second binaire qui le demande est refusé, avec le nom, la version et la session de celui qui le tient. Pour servir les mêmes projets depuis un second binaire, on lui donne un décalage de port à l'installation — `DSVS_PORT_SHIFT=100 ./build.sh --install`, ou `dsvs-x --install-unit --port-shift 100` — que son unité garde d'une réinstallation et d'une mise à jour à l'autre : ses façades répondent alors au port déclaré plus 100.

Un binaire construit depuis un clone se met à jour depuis ce clone : `dsvs update` récupère les commits, reconstruit, remplace et relance, en disant chaque étape. Un clone qui porte des modifications non commitées refuse, parce que ce qu'il construirait n'aurait pas de version qui le distingue ; un clone laissé sur une autre branche aussi, parce que `dsvs-next` ne s'installe pas sous le nom `dsvs`. Mettre à jour depuis un clone construit et lance ce que porte l'amont : **qui peut pousser sur cette branche exécute du code sur votre machine**. C'est le dessein, et c'est ce que fait `git pull && ./build.sh --install` à la main.

[CONTRIBUTING.md](CONTRIBUTING.md) couvre les tests, les hooks git et les conventions de commit.

## Utilisation

La liste des commandes et des options est la sortie de `dsvs help`, reproduite dans la [section Usage du README anglais](README.md#usage).

`dsvs help <commande>` détaille une commande et ses options ; `?` liste les touches de la TUI. `dsvs skill > ~/.claude/skills/dsvs/SKILL.md` installe un skill qui apprend dsvs à un agent de code, tiré de la même table de commandes.

### L'icône et le panneau

L'icône ouvre le panneau ; son infobulle donne le binaire et sa version. Les projets ouverts y restent listés d'une ouverture de session à l'autre, à froid, chacun avec trois gestes : démarrer ou arrêter ses apps, ouvrir sa TUI ou la rappeler à l'écran, ou l'oublier. Chaque app peut être démarrée, arrêtée et relancée, et chaque façade s'ouvre dans le navigateur. En bas, deux gestes distincts : arrêter les apps de tous les projets, ou quitter dsvs, icône comprise ; puis la mise à jour, et la ligne de la beta décrite plus haut.

## Configuration

Le dossier du projet porte un `config.dsvs.ts` — du code, que node évalue une fois — ou un `dsvs.json`, quand il n'a pas de raison d'exiger node. Les deux suivent le même contrat : l'extension décide de qui lit le fichier, et rien en aval ne sait duquel il vient. dsvs le cherche en remontant depuis le dossier courant ; `--config` en nomme un autre.

Un `dsvs.json` minimal, une façade et une app :

```json
{
  "port": 4200,
  "ingress": [
    {
      "host": "shop.localhost",
      "routes": [
        { "path": "/", "app": "front" },
        { "path": "/api", "proxy": { "target": "https://staging.example.com", "changeOrigin": true } }
      ]
    }
  ],
  "apps": [
    { "name": "front", "command": ["npm", "run", "start", "--", "--port", "{port}"] }
  ]
}
```

`dsvs start front`, puis ouvrez `http://shop.localhost:4200/`. Les navigateurs résolvent `*.localhost` tout seuls : pas de `/etc/hosts`, pas de sudo.

Le même en TypeScript, dans un `config.dsvs.ts` :

```ts
import type { Config } from "dsvs";

export default {
  port: 4200,
  ingress: [/* ... */],
  apps: [/* ... */],
} satisfies Config;
```

Le fichier est du code : il peut lire un manifeste, interroger le workspace, ou importer un fichier de cibles resté hors de git. `DSVS_CONFIG` porte son chemin absolu pendant l'évaluation, de quoi se situer sans rien deviner. L'`export default` peut aussi être une fonction, `async` comprise.

Node retire les types sans les vérifier : `satisfies Config` ne protège que dans l'éditeur. dsvs vérifie donc la forme de ce que le fichier construit, et le refuse comme une faute de syntaxe — la configuration d'avant continue de tourner — pour une clé qu'il ne lit pas, en proposant celle que vous vouliez sans doute dire, comme pour une valeur du mauvais type, chacune par son chemin : `apps[1]: unknown key "evn" (did you mean "env"?)`, `port: a number, not "4380"`. Un dsvs plus ancien le dit de même d'une clé qu'il ne connaît pas encore.

### Les types TypeScript

Le paquet `types/` déclare chaque champ et le documente : c'est la référence. Il ne contient que des déclarations : le code de `form()` et de ses règles vient du binaire, qui le sert sous le nom `"dsvs"` à la configuration et à ce qu'elle importe. Le paquet n'est donc pas nécessaire pour que la configuration tourne, et un projet l'installe en dépendance optionnelle, épinglée sur le tag du binaire :

```sh
pnpm add --save-optional "dsvs@$(dsvs types)"
```

Le spécificateur est une URL `git+ssh` sur ce dépôt : il faut donc une clé SSH que GitHub connaît. Sans elle, pnpm installe tout le reste et sort en succès ; seul l'éditeur montre que les types manquent, par un import non résolu. Ce que pnpm en dit dépend de sa version et du lockfile : une ligne d'information, un avertissement qui porte l'erreur de git, ou rien du tout. pnpm 11 n'écrit même pas dans `package.json` une dépendance qu'il n'a pas pu résoudre : `pnpm add` ne change alors rien, et c'est depuis une machine qui a la clé qu'on ajoute les types. La CI du projet installe avec `pnpm install --frozen-lockfile --no-optional` et sort `*.dsvs.ts` de son tsconfig de build, pour ne pas typer un import absent.

### Référence

`Config`, l'export par défaut :

| Clé | Type | |
|---|---|---|
| `port` | number | Le seul port ouvert sur la machine ; chaque façade y répond. |
| `ingress` | `Ingress[]` | Les façades, une par hôte. Plusieurs peuvent mener à la même app. |
| `apps` | `App[]` | |
| `groups` | `{ name, members }[]` | Des ensembles qu'on démarre par leur nom et qu'on lit d'une ligne, leurs journaux fusionnés. `members` sont des noms d'apps, chacun une fois ; un groupe ne contient ni façade ni groupe. |
| `subnet` | string | La plage des adresses virtuelles des apps. Défaut `10.77.0.0/24` ; à changer si elle entre en collision avec un VPN. La première adresse revient au portier. |
| `watch` | string[] | Les chemins dont une sauvegarde fait relire la configuration, en plus de `--watch`. Un dossier surveille ses fichiers `*.dsvs.ts`, un fichier lui-même. Défaut : le dossier de la configuration. Un chemin qui n'existe pas est refusé. |

`Ingress`, une façade :

| Clé | Type | |
|---|---|---|
| `host` | string | L'hôte qui désigne la façade, `"shop.localhost"` par exemple. Omis, la façade répond pour tout ce qu'aucune autre ne réclame. |
| `name` | string | Son nom dans les interfaces et les journaux. Défaut : son hôte. Jamais le nom d'une app. |
| `routes` | `Route[]` | Ce que devient chaque chemin. Le plus spécifique gagne, quel que soit l'ordre, et `"/"` prend le reste ; sans lui, un chemin sans route est un 404. |

`Route` : un `path` — un préfixe sans barre finale, si bien qu'un `baseHref` s'écrit tel quel — et exactement une de ces clés :

| Clé | Type | |
|---|---|---|
| `app` | string | L'app qui reçoit ces chemins. |
| `proxy` | `{ target, secure?, changeOrigin? }` | Une cible distante, écrite comme une entrée du `proxy.conf.json` d'Angular, mêmes mots, mêmes défauts. `target` est une URL http ou https dont le chemin préfixe celui de la requête. `secure: false` accepte un certificat qu'on ne peut pas vérifier. `changeOrigin: true` envoie l'hôte de la cible au lieu de celui du navigateur, ce que veut tout vhost distant. |
| `redirect` | string | Une 302 vers ce chemin. Elle ne reporte pas la suite du chemin demandé. |

`App` :

| Clé | Type | |
|---|---|---|
| `name` | string | |
| `command` | string[] | Argv, exécuté sans shell, dans un terminal que dsvs lui prête. Une app ne choisit pas son adresse : dsvs lui en réserve une et remplace `{port}` et `{ip}`, ici comme dans `env`. `{port}` est obligatoire : sans lui, rien ne saurait où joindre l'app. |
| `env` | `Record<string, string \| undefined>` | Les variables posées par-dessus l'environnement de la session. Une valeur `undefined` n'en pose aucune. |
| `dir` | string | Le dossier de travail, absolu ou relatif à la configuration. Défaut : son dossier. |
| `color` | string | Sa couleur dans les journaux : `red`, `green`, `yellow`, `blue`, `magenta` ou `cyan`. Défaut : prise dans une palette, dans l'ordre. |

### Les paramètres : un formulaire

Une configuration peut déclarer des **paramètres** : un formulaire, que la construction consomme. `form(modèle, schéma)` déclare les champs par leur défaut — un booléen fait un flag, un nombre un nombre, un texte un texte, une liste un choix dont le défaut est en tête, un objet un groupe — et le schéma pose leurs règles ; `config(formulaire, construction)` construit la configuration à partir des valeurs résolues, une fois le formulaire valide. Les règles sont celles des Signal Forms d'Angular — `hidden`, `disabled`, `readonly`, `required`, `min`, `max`, `minLength`, `maxLength`, `pattern`, `validate`, `validateTree`, `apply`, `applyWhen` — plus quatre : `choices` et `defaultValue` donnent les choix et le défaut du moment (un choix peut être désactivé avec sa raison, et reste à vue), `keep` retient une valeur choisie pour le projet, et `network` la retient pour le réseau par où la machine sort. Le schéma reçoit aussi le **réseau observé** : les interfaces levées et les routes de sortie.

L'exemple d'une boutique, dont l'API vient du serveur local ou d'un serveur déployé (le code garde ses commentaires en anglais) :

```ts
import { choices, config, defaultValue, disabled, form, hidden, keep, max, min, network, validateTree } from "dsvs";
import servers from "./servers.dsvs.ts";

// The front of a shop: its API comes from the local server (the api app) or from a deployed one,
// staging — reachable from the company network — or demo, public.
const params = form(
  { api: ["local", "staging", "demo"], deployed: { version: ["v2", "v3"], traces: false }, slowApi: false, products: 20, locale: ["en", "fr"] },
  (p, observed) => {
    keep(p.locale);

    // The server to reach depends on the network: chosen once per network, it comes back with it.
    network(p.api);

    // staging is only reachable from the company network, through its tunnel, and so are its traces.
    const atWork = observed?.links.some((link) => link.name.startsWith("tun")) ?? false;
    choices(p.api, ["local", { value: "staging", disabled: !atWork && "staging is only reachable from the company network" }, "demo"]);
    defaultValue(p.api, atWork ? "staging" : "demo");
    hidden(p.deployed, { when: ({ valueOf }) => valueOf(p.api) === "local" });
    disabled(p.deployed.traces, { when: !atWork && "traces are only reachable from the company network" });

    // What the local server serves is only set when it is the one reached.
    hidden(p.slowApi, { when: ({ valueOf }) => valueOf(p.api) !== "local" });
    hidden(p.products, { when: ({ valueOf }) => valueOf(p.api) !== "local" });
    min(p.products, 0);
    max(p.products, 5000);

    validateTree(p, ({ valueOf }) =>
      valueOf(p.locale) === "fr" && valueOf(p.api) === "demo" ? { field: p.locale, message: "demo has no fr build" } : null,
    );
  },
);

export default config(params, ({ api, deployed, slowApi, products, locale }) => ({
  port: 4380,
  ingress: [
    {
      host: "shop.localhost",
      routes: [
        { path: "/", app: locale === "fr" ? "front-fr" : "front" },
        { path: "/admin", app: "admin" },
        { path: "/api", ...(api === "local" ? { app: "api" } : { proxy: { target: `${servers[api]}/${deployed.version}`, changeOrigin: true } }) },
      ],
    },
  ],
  apps: [
    // A hidden field is undefined here: products and slowApi only serve the local server, traces only a deployed one.
    { name: "api", command: ["node", "api.mjs", "--port={port}", ...(api === "local" ? [`--products=${products}`, ...(slowApi ? ["--slow"] : [])] : [])] },
    { name: "front", command: ["pnpm", "start", "--port={port}"], env: { API_TRACES: String(api !== "local" && deployed.traces) }, reload: { stdin: "r\n" } },
    /* front-fr, admin… */
  ],
}));
```

**D'où vient une valeur.** Une valeur est la première intention qui existe : celle retenue pour le réseau si le champ le suit, celle de la session — choisie, ou ramenée par le dernier réseau qui en avait une —, et celle retenue pour le projet s'il la garde. Elle vaut si elle est parmi les choix du moment et qu'aucune règle ne la désactive ; sinon le défaut répond — ou le premier choix actif —, et l'intention revient dès qu'elle redevient possible. Sur un réseau qui n'a rien retenu, la valeur en vigueur reste. Une valeur retenue, c'est tout ce qui n'est pas le geste en cours ; celle que les règles de son champ ne prennent plus, comme un `max` abaissé sous elle, **cède** : au bord qu'elle dépasse pour `min` et `max`, au défaut sinon. La configuration s'applique, l'intention reste en mémoire, et dsvs le dit tant que ça dure, au journal : `products=20 no longer holds (at most 15): products=15 until it does`. Une valeur retenue fait lister le projet, comme `dsvs remember`, et `dsvs forget` l'oublie avec lui.

**Les champs cachés.** Un champ caché n'a pas de valeur pour la construction : il y vaut `undefined`, tous les champs d'un groupe caché aussi, et la construction ne doit pas s'en servir — `api === "local" ? products : 0`. Les règles, elles, voient toujours la valeur qu'il garde, et qu'il retrouve en réapparaissant. Les types restent ceux du modèle, puisque le schéma est fixe : `api === "stagin"`, `p.deployed.verison` ou un choix hors de la liste sont des erreurs de type, un champ caché lu sans garde n'en est pas une. dsvs le rattrape : quand un champ est caché, il construit une seconde fois avec sa valeur gardée, et si le résultat diffère, la configuration s'en sert quand même et dsvs le dit comme une valeur qui cède — `slowApi, products are hidden, yet apps.api.command[3] depends on them` —, la configuration retenue restant celle construite sans.

Sans formulaire, l'export par défaut reste un objet, ou une fonction qui ignore ses arguments. `export const params` est l'ancienne forme, et elle est refusée.

### N'importe quel programme est une configuration

Node n'est qu'une implémentation d'un contrat que tout exécutable peut remplir, passé par `--config` : `.json` est une configuration nue, `.ts`, `.mts`, `.js` et `.mjs` passent par node, tout autre fichier est exécuté lui-même. Il reçoit dans `DSVS_INPUT` une ligne de JSON — les intentions couche par couche, en texte, flags compris, et `observed`, `null` quand rien n'observe le réseau :

```json
{"asked":{"deployed.version":"v3"},"network":{"api":"staging"},"session":{"slowApi":"true"},"kept":{"locale":"en"},"observed":{"links":[{"name":"tun0","type":"tuntap","addrs":["10.8.0.2/24"]}],"exits":[{"via":"198.51.100.1","dev":"eth0"}]}}
```

`DSVS_CONFIG` porte son chemin, et son dossier courant est celui du fichier. Il écrit sur stdout `{"form":{"fields":[…],"errors":[…],"notices":[…]},"config":{…}}` : chaque champ résolu, dans l'ordre — `name`, `type` (`choice`, `flag`, `text` ou `number`), `choices`, `disabledChoices` avec chaque raison, `value`, `from` (`asked`, `network`, `session`, `kept` ou `default`), ses états `hidden`, `disabled` et ses `reasons`, `readonly`, `keep`, `network`, `errors`, et `yielded`, la valeur retenue qui ne tient plus, avec sa couche et ses erreurs —, puis `notices`, ce que le formulaire dit sans rien refuser, et la configuration que ces valeurs construisent, `null` quand le formulaire a une erreur. Ce qu'il écrit sur stderr va au journal de la session. Un code de sortie non nul, un JSON illisible ou dix secondes sans réponse sont une erreur qui ne change rien. Un `dsvs.json` est une configuration nue, sans formulaire.

### Sauvegarder suffit

Les façades, les routes, les cibles de proxy et les groupes sont relus sans rien redémarrer. Le port aussi : la session passe sur le nouveau et quitte l'ancien, ou, s'il est pris, refuse la configuration et dit qui le tient. Ce qui est vivant — le sous-réseau, les processus — reste tel quel, et le journal le dit. dsvs surveille le fichier de configuration lui-même, quel que soit son nom, les chemins qu'il déclare (`watch`), ceux de la ligne de commande (`--watch`), et à défaut son dossier : un import qui vit ailleurs doit être déclaré, sinon le sauvegarder ne recharge rien.

Les apps suivent la déclaration, sans qu'aucun processus ne soit coupé sous les pieds de qui s'en sert. Une app déclarée en cours de session apparaît, prête à démarrer. Une app dont la commande a changé se marque `↻ restart pending` : elle tourne avec la précédente, et c'est son redémarrage qui prend la nouvelle. Une app retirée de la configuration se marque `⊘ gone from the configuration` : elle tourne tant qu'on la laisse tourner, repart avec ce qu'elle avait, et quitte la liste une fois arrêtée. Son journal se lit jusqu'à la fin de la session : `dsvs logs worker` le montre, terminé par son départ. La remettre en fait de nouveau une app ordinaire, à redémarrer, qui reprend ce journal.

## Ce qui tourne pendant une session

Par défaut, les apps n'ouvrent **aucun port** sur la machine : elles vivent sur des adresses virtuelles dans un namespace réseau sans privilèges, dont le trafic et le DNS sortent par la pile de la machine elle-même, VPN et proxy système compris. `--net loopback` s'en passe, `--net netns` l'exige au lieu de retomber en silence. La session se réexécute dans une unité systemd éphémère, sa boîte : un arrêt brutal ne laisse alors aucun processus derrière, pas même ce qu'un runner détache. Sans session systemd, elle tourne sans boîte, le dit, et cette garantie-là n'est plus.

`dsvs config` écrit la configuration telle que dsvs la voit — défauts appliqués, routes ordonnées — dans le format JSON qu'il sait relire. Une configuration à formulaire sort construite de ce qu'aurait une session ouverte ici, plus les valeurs demandées par-dessus, sans son formulaire : `dsvs config api=staging` donne l'instantané de la boutique sur staging. `dsvs config --session` écrit celle qui est en vigueur dans la session qui tourne, et `--params --session` dit d'où vient chaque valeur.

## Comment c'est construit

Chaque paquet porte ses décisions, ce qui a été essayé puis écarté, et ses limites connues dans son commentaire de paquet : `go doc ./internal/hub`, `go doc ./internal/core`, `go doc ./cmd/dsvs`, et ainsi de suite. Les coutures entre eux :

- `config` charge et valide, et rien en aval ne revérifie ; `core` supervise ; `gateway` sert ; `hub` tient le port et le répartit ; `fdpass` fait voyager les descripteurs de fichier, par une socket comme à travers un exec, et porte le cadrage des lignes du protocole ; `ui/*` consomment des instantanés et un flux d'événements qui peut déborder sans perdre un réveil — les lignes de journal, elles, peuvent tomber, et le flux dit combien.
- `core.Network` dit où les apps écoutent, `core.Front` qui tient le port : deux questions, deux interfaces. `core.Gateway` et `core.Network` sont les points de remplacement — un nginx piloté, loopback ou netns — et une interface web ne serait qu'un consommateur de plus du core.

## Contribuer

Les issues et les pull requests sont les bienvenues. [CONTRIBUTING.md](CONTRIBUTING.md) explique comment construire et tester, les conventions de commit et la façon dont se font les releases ; [SECURITY.md](SECURITY.md) comment signaler une vulnérabilité.

## Licence

dsvs est publié sous [licence MIT](LICENSE).
### N'importe quel programme est une configuration

Node n'est qu'une implémentation d'un contrat que tout exécutable peut remplir, passé par `--config` : `.json` est une configuration nue, `.ts`, `.mts`, `.js` et `.mjs` passent par node, tout autre fichier est exécuté lui-même. Il reçoit dans `DSVS_INPUT` une ligne de JSON — les intentions couche par couche, en texte, flags compris, et `observed`, `null` quand rien n'observe le réseau :

```json
{"asked":{"deployed.version":"v3"},"network":{"api":"staging"},"session":{"slowApi":"true"},"kept":{"locale":"en"},"observed":{"links":[{"name":"tun0","type":"tuntap","addrs":["10.8.0.2/24"]}],"exits":[{"via":"198.51.100.1","dev":"eth0"}]}}
```

`DSVS_CONFIG` porte son chemin, et son dossier courant est celui du fichier. Il écrit sur stdout `{"form":{"fields":[…],"errors":[…],"notices":[…]},"config":{…}}` : chaque champ résolu, dans l'ordre — `name`, `type` (`choice`, `flag`, `text` ou `number`), `choices`, `disabledChoices` avec chaque raison, `value`, `from` (`asked`, `network`, `session`, `kept` ou `default`), ses états `hidden`, `disabled` et ses `reasons`, `readonly`, `keep`, `network`, `errors`, et `yielded`, la valeur retenue qui ne tient plus, avec sa couche et ses erreurs —, puis `notices`, ce que le formulaire dit sans rien refuser, et la configuration que ces valeurs construisent, `null` quand le formulaire a une erreur. Ce qu'il écrit sur stderr va au journal de la session. Un code de sortie non nul, un JSON illisible ou dix secondes sans réponse sont une erreur qui ne change rien. Un `dsvs.json` est une configuration nue, sans formulaire.

### Sauvegarder suffit

Les façades, les routes, les cibles de proxy et les groupes sont relus sans rien redémarrer. Ce qui est vivant — le port, le sous-réseau, les processus — reste tel quel, et le journal le dit. dsvs surveille le fichier de configuration lui-même, quel que soit son nom, les chemins qu'il déclare (`watch`), ceux de la ligne de commande (`--watch`), et à défaut son dossier : un import qui vit ailleurs doit être déclaré, sinon le sauvegarder ne recharge rien.

Les apps suivent la déclaration, sans qu'aucun processus ne soit coupé sous les pieds de qui s'en sert. Une app déclarée en cours de session apparaît, prête à démarrer. Une app dont la commande a changé se marque `↻ restart pending` : elle tourne avec la précédente, et c'est son redémarrage qui prend la nouvelle. Une app retirée de la configuration se marque `⊘ gone from the configuration` : elle tourne tant qu'on la laisse tourner, repart avec ce qu'elle avait, et quitte la liste une fois arrêtée. La remettre en fait de nouveau une app ordinaire, à redémarrer.

## Ce qui tourne pendant une session

Par défaut, les apps n'ouvrent **aucun port** sur la machine : elles vivent sur des adresses virtuelles dans un namespace réseau sans privilèges, dont le trafic et le DNS sortent par la pile de la machine elle-même, VPN et proxy système compris. `--net loopback` s'en passe, `--net netns` l'exige au lieu de retomber en silence. La session se réexécute dans une unité systemd éphémère, sa boîte : un arrêt brutal ne laisse alors aucun processus derrière, pas même ce qu'un runner détache. Sans session systemd, elle tourne sans boîte, le dit, et cette garantie-là n'est plus.

`dsvs config` écrit la configuration telle que dsvs la voit — défauts appliqués, routes ordonnées — dans le format JSON qu'il sait relire. Une configuration à formulaire sort construite de ce qu'aurait une session ouverte ici, sans son formulaire.

## Comment c'est construit

Chaque paquet porte ses décisions, ce qui a été essayé puis écarté, et ses limites connues dans son commentaire de paquet : `go doc ./internal/hub`, `go doc ./internal/core`, `go doc ./cmd/dsvs`, et ainsi de suite. Les coutures entre eux :

- `config` charge et valide, et rien en aval ne revérifie ; `core` supervise ; `gateway` sert ; `hub` tient le port et le répartit ; `fdpass` fait voyager les descripteurs de fichier, par une socket comme à travers un exec, et porte le cadrage des lignes du protocole ; `ui/*` consomment des instantanés et un flux d'événements qui peut déborder sans perdre un réveil — les lignes de journal, elles, peuvent tomber, et le flux dit combien.
- `core.Network` dit où les apps écoutent, `core.Front` qui tient le port : deux questions, deux interfaces. `core.Gateway` et `core.Network` sont les points de remplacement — un nginx piloté, loopback ou netns — et une interface web ne serait qu'un consommateur de plus du core.

## Contribuer

Les issues et les pull requests sont les bienvenues. [CONTRIBUTING.md](CONTRIBUTING.md) explique comment construire et tester, les conventions de commit et la façon dont se font les releases ; [SECURITY.md](SECURITY.md) comment signaler une vulnérabilité.

## Licence

dsvs est publié sous [licence MIT](LICENSE).
