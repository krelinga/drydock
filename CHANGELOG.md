# Changelog

## [0.7.1](https://github.com/krelinga/drydock/compare/v0.7.0...v0.7.1) (2026-10-09)


### Bug Fixes

* run provision's jobs on life.Group, and leave a paused workspace parked at boot ([#106](https://github.com/krelinga/drydock/issues/106)) ([a8a4a30](https://github.com/krelinga/drydock/commit/a8a4a300f33391388e04361cf286d240aa615da3))

## [0.7.0](https://github.com/krelinga/drydock/compare/v0.6.1...v0.7.0) (2026-10-09)


### Features

* the port registry: enable, open and disable a preview from the workspace page ([#97](https://github.com/krelinga/drydock/issues/97)) ([7598d41](https://github.com/krelinga/drydock/commit/7598d41a455b0f974ac0096f145fd5cb182f1615))


### Bug Fixes

* unpause a paused workspace before stopping its session server ([#96](https://github.com/krelinga/drydock/issues/96)) ([975445d](https://github.com/krelinga/drydock/commit/975445d94acb6baa1f3a8778e1aacfeca1196ca1))

## [0.6.1](https://github.com/krelinga/drydock/compare/v0.6.0...v0.6.1) (2026-10-09)


### Bug Fixes

* accept the daemon's default log configuration on a guarded start ([#94](https://github.com/krelinga/drydock/issues/94)) ([4fa98cb](https://github.com/krelinga/drydock/commit/4fa98cba253259115d86fb53a13df53f1d466cfb))
* make the CLI's empty build context before the docker guard checks it ([#91](https://github.com/krelinga/drydock/issues/91)) ([317f8e2](https://github.com/krelinga/drydock/commit/317f8e294e17c1da53d6c70d2ce71b82b4e5e470))
* never call a paused container's session server stopped ([#93](https://github.com/krelinga/drydock/issues/93)) ([aa51a58](https://github.com/krelinga/drydock/commit/aa51a58391223673768996eebb31927b9974e489))

## [0.6.0](https://github.com/krelinga/drydock/compare/v0.5.0...v0.6.0) (2026-10-09)


### Features

* name the cause and the fix for every §12 failure the UI can show ([#79](https://github.com/krelinga/drydock/issues/79)) ([011cc6b](https://github.com/krelinga/drydock/commit/011cc6ba4bde258213054994d46d42150b1067bc))
* proxy a preview to the container's dev server, websockets included ([#87](https://github.com/krelinga/drydock/issues/87)) ([1bef223](https://github.com/krelinga/drydock/commit/1bef2237937a76674132946456aceebc5168c754))
* show memory and disk on each workspace, and refuse new work on a full disk ([#75](https://github.com/krelinga/drydock/issues/75)) ([5b8575c](https://github.com/krelinga/drydock/commit/5b8575c74c77b2e3f6c91e3029f28016420ee76a))
* sign a device into a preview with a one-time token and a host-only cookie ([#82](https://github.com/krelinga/drydock/issues/82)) ([d5bcb80](https://github.com/krelinga/drydock/commit/d5bcb8081eaa5be7574216e7530ef13f0b67a3f6))


### Bug Fixes

* cancel and wait for a triggered catalog refresh at shutdown ([#83](https://github.com/krelinga/drydock/issues/83)) ([ae90c88](https://github.com/krelinga/drydock/commit/ae90c889f9ad5520056494aafdbf786c11fd0143))
* check the docker arguments devcontainer actually runs against the approval ([#78](https://github.com/krelinga/drydock/issues/78)) ([8bb408d](https://github.com/krelinga/drydock/commit/8bb408d1b9bc7491e9075ed9ad31a213919b8f09))
* settle "Check now" even when the Claude login hasn't changed ([#85](https://github.com/krelinga/drydock/issues/85)) ([f417657](https://github.com/krelinga/drydock/commit/f4176579258c3455d709f4278fe16d902883cbaa))
* settle "Restart session server" when the old server won't stop ([#86](https://github.com/krelinga/drydock/issues/86)) ([d24e350](https://github.com/krelinga/drydock/commit/d24e350fa65929fb6fe6d7cdcf94bac2327afa8f))

## [0.5.0](https://github.com/krelinga/drydock/compare/v0.4.5...v0.5.0) (2026-10-08)


### Features

* serve the preview front door — every preview URL answers 401 and none reaches the API ([#76](https://github.com/krelinga/drydock/issues/76)) ([d5faf61](https://github.com/krelinga/drydock/commit/d5faf61e040de1599713fea65a4f01669d6d9b1a))

## [0.4.5](https://github.com/krelinga/drydock/compare/v0.4.4...v0.4.5) (2026-10-08)


### Bug Fixes

* stop warning that an 8-hour access token is the login expiring ([#74](https://github.com/krelinga/drydock/issues/74)) ([7739cf9](https://github.com/krelinga/drydock/commit/7739cf9866f65a1de7d5f5cdf4ea5a744612576e))

## [0.4.4](https://github.com/krelinga/drydock/compare/v0.4.3...v0.4.4) (2026-10-08)


### Bug Fixes

* close a step left started when Drydock stopped mid-step, at boot ([#72](https://github.com/krelinga/drydock/issues/72)) ([f4f6e11](https://github.com/krelinga/drydock/commit/f4f6e1101b4f5625d646330add823b8833a51184))

## [0.4.3](https://github.com/krelinga/drydock/compare/v0.4.2...v0.4.3) (2026-10-07)


### Bug Fixes

* close the low-severity findings from the post-merge review ([#63](https://github.com/krelinga/drydock/issues/63)) ([fdceed9](https://github.com/krelinga/drydock/commit/fdceed97f1d9deb7caa5ea8116ab544fd4e89d17))
* never leave a login container behind after a cancel, and stop the login PTY test racing docker ps ([#65](https://github.com/krelinga/drydock/issues/65)) ([de507f9](https://github.com/krelinga/drydock/commit/de507f9418eb686030b163e574684971fd02ae7d))

## [0.4.2](https://github.com/krelinga/drydock/compare/v0.4.1...v0.4.2) (2026-10-07)


### Bug Fixes

* bound identity checks, fix the credential volume's owner, and let an expired access token refresh ([#61](https://github.com/krelinga/drydock/issues/61)) ([ce7dcef](https://github.com/krelinga/drydock/commit/ce7dcef88d9130b93470b98ab93d0aeaf48bf077))
* mount each workspace's broker socket directory so a restart does not strand it ([#59](https://github.com/krelinga/drydock/issues/59)) ([2f4cf85](https://github.com/krelinga/drydock/commit/2f4cf85812908555fe4c83ac77be3fca66088180))
* never leave the old binary running after a refused key, and pin the docs to a real release ([#62](https://github.com/krelinga/drydock/issues/62)) ([4c313b0](https://github.com/krelinga/drydock/commit/4c313b09cfc14cea60408d0be08e4ac4cea738e4))
* redact every supervisor log line and drop a removed repository's grants ([#56](https://github.com/krelinga/drydock/issues/56)) ([65a13ce](https://github.com/krelinga/drydock/commit/65a13ceb9c45bd353817de019e27325f9ffb8ab5))
* require operator approval before a dev container config reaches the host ([#58](https://github.com/krelinga/drydock/issues/58)) ([cddbab3](https://github.com/krelinga/drydock/commit/cddbab3c40d31cc0584e227a9ae9c88bf66e8af6))
* settle in-flight actions after a resync and refuse a secret create that would overwrite ([#60](https://github.com/krelinga/drydock/issues/60)) ([c6d117e](https://github.com/krelinga/drydock/commit/c6d117ee5e6e1b566b696a03ac13990071ab6091))

## [0.4.1](https://github.com/krelinga/drydock/compare/v0.4.0...v0.4.1) (2026-10-06)


### Bug Fixes

* never close an event subscription twice when it starts after shutdown ([#54](https://github.com/krelinga/drydock/issues/54)) ([2a50c0e](https://github.com/krelinga/drydock/commit/2a50c0ee4c49b7660553dffaff0523b7f540d1fb))

## [0.4.0](https://github.com/krelinga/drydock/compare/v0.3.0...v0.4.0) (2026-10-06)


### Features

* install Claude Code pinned in the Feature and create the shared credential volume ([#38](https://github.com/krelinga/drydock/issues/38)) ([b57e22a](https://github.com/krelinga/drydock/commit/b57e22a06551ba51a666d4b6977b934bec555d61))
* sign the shared volume into Claude Code from the UI ([#53](https://github.com/krelinga/drydock/issues/53)) ([f975570](https://github.com/krelinga/drydock/commit/f975570cda91093970818e7e55ee5d62a25b1986))
* supervise one remote-control server per workspace and discover its sessions ([#52](https://github.com/krelinga/drydock/issues/52)) ([ec84ceb](https://github.com/krelinga/drydock/commit/ec84cebb881956220f4817aee820a37b73f15b3c))
* watch the shared Claude login and show it fleet-wide ([#37](https://github.com/krelinga/drydock/issues/37)) ([4ab4fc1](https://github.com/krelinga/drydock/commit/4ab4fc19b186497e74c69102294ba6234c2c8757))


### Bug Fixes

* tell a GitHub App permission the broker lacks apart from a revoked repository ([#49](https://github.com/krelinga/drydock/issues/49)) ([069e6f4](https://github.com/krelinga/drydock/commit/069e6f4a9659533458bbbb272934a7b5fac08a81))

## [0.3.0](https://github.com/krelinga/drydock/compare/v0.2.1...v0.3.0) (2026-10-06)


### Features

* let the installer take the secrets master key from a file ([#46](https://github.com/krelinga/drydock/issues/46)) ([fd7ddf9](https://github.com/krelinga/drydock/commit/fd7ddf9b883445cfb06f960ab8810e396c0b85ef))


### Bug Fixes

* send Drydock's real Origin from the UI under its no-referrer policy ([#47](https://github.com/krelinga/drydock/issues/47)) ([fec2486](https://github.com/krelinga/drydock/commit/fec2486f80c9dfeba0478f40c30d637f5e91b1c9))

## [0.2.1](https://github.com/krelinga/drydock/compare/v0.2.0...v0.2.1) (2026-10-06)


### Bug Fixes

* create /etc/drydock before installing the App key on a first install ([#44](https://github.com/krelinga/drydock/issues/44)) ([afdd3f7](https://github.com/krelinga/drydock/commit/afdd3f782f78f5fe9f2bb105172d4b382751caf3))

## [0.2.0](https://github.com/krelinga/drydock/compare/v0.1.0...v0.2.0) (2026-10-05)


### Features

* add the clone button, the workspace card's state, and the workspace detail view ([#23](https://github.com/krelinga/drydock/issues/23)) ([89ec1c8](https://github.com/krelinga/drydock/commit/89ec1c8935f7a1ac1992a2236d6c7f5e01829625))
* add the drydock devcontainer Feature's GitHub half ([#17](https://github.com/krelinga/drydock/issues/17)) ([02ec0f4](https://github.com/krelinga/drydock/commit/02ec0f4395ce63b7673ca69fe43bd405c475505d))
* add the Phase 2 skeleton: event stream, workspace states, boot reconciliation ([#12](https://github.com/krelinga/drydock/issues/12)) ([5aa483c](https://github.com/krelinga/drydock/commit/5aa483c59974581f2c39a4b9854059c10fef6f9e))
* add the secrets UI — list, write-only form, grants, rotate result ([#29](https://github.com/krelinga/drydock/issues/29)) ([5129da0](https://github.com/krelinga/drydock/commit/5129da0425bdf46abcabe618021d030c1c78455f))
* add the Stop, Rebuild and Delete buttons, with the typed-name confirm ([#33](https://github.com/krelinga/drydock/issues/33)) ([d7cc131](https://github.com/krelinga/drydock/commit/d7cc1316aa4257c80952f720f0e5e5586f071855))
* add the stream store, the reducer, and the repository catalog on the home screen ([#20](https://github.com/krelinga/drydock/issues/20)) ([dedff88](https://github.com/krelinga/drydock/commit/dedff8855ead9b89209cfbf4cc07f55b447d2bae))
* add the token broker and its in-container git and gh clients ([#16](https://github.com/krelinga/drydock/issues/16)) ([830b426](https://github.com/krelinga/drydock/commit/830b4264a4ab44e110c538e796271282e7d14aec))
* clone a workspace's repository with no token left behind ([#18](https://github.com/krelinga/drydock/issues/18)) ([f02e1dc](https://github.com/krelinga/drydock/commit/f02e1dc1b04590ea87c1f04733cdb089c26a84ec))
* list the GitHub App's repositories ([#14](https://github.com/krelinga/drydock/issues/14)) ([3b36855](https://github.com/krelinga/drydock/commit/3b368556740bed66c822b4bf48540f428a0ca05d))
* provision a workspace end to end behind POST /api/workspaces ([#25](https://github.com/krelinga/drydock/issues/25)) ([dac8ba6](https://github.com/krelinga/drydock/commit/dac8ba673412fef260675a6b7c2b12ba6e0df41f))
* stop, rebuild and delete a workspace ([#30](https://github.com/krelinga/drydock/issues/30)) ([c54d69d](https://github.com/krelinga/drydock/commit/c54d69d7325079d8a8e3d617a8f4fdc26ce498f7))
* store repository secrets and deliver them before every command ([#24](https://github.com/krelinga/drydock/issues/24)) ([09bd1fd](https://github.com/krelinga/drydock/commit/09bd1fd1bea5c50bb9ef029b3f6d68956d9dd748))


### Bug Fixes

* align the workspace UI with the provisioning backend as built ([#26](https://github.com/krelinga/drydock/issues/26)) ([fd7b0c7](https://github.com/krelinga/drydock/commit/fd7b0c7780699f5deca71cf0772cb8d291379435))
* answer a wrong method with method_not_allowed, not bad_request ([08b5b6b](https://github.com/krelinga/drydock/commit/08b5b6b8d5cb577b7b13504f7a342d97245ab2d1))
* close the gaps the parallel agents found ([#22](https://github.com/krelinga/drydock/issues/22)) ([6c75319](https://github.com/krelinga/drydock/commit/6c75319be691d1d3b6585c3b2306dafa3d74a246))
* fail the secrets prelude closed when the helper cannot run ([#27](https://github.com/krelinga/drydock/issues/27)) ([cc1d0da](https://github.com/krelinga/drydock/commit/cc1d0da34d8a961fac24801d79b2e69d7e6f9fad))
* honour a committed devcontainer-lock.json as VS Code does ([#28](https://github.com/krelinga/drydock/issues/28)) ([ff4a705](https://github.com/krelinga/drydock/commit/ff4a70595bfa4c9e4d00e855b5d698ef3b61df85))
* keep a failed stop and the cap readable after a reload, and sweep stray cleanup helpers at boot ([#39](https://github.com/krelinga/drydock/issues/39)) ([af06fef](https://github.com/krelinga/drydock/commit/af06fefa381fa111163b402c26ac499cb45670c0))
* remove root-owned files on delete, and close a failed run's socket ([#32](https://github.com/krelinga/drydock/issues/32)) ([41fa3fe](https://github.com/krelinga/drydock/commit/41fa3fe04403139a357365d06f5bcbedfdcbe176))
* report undeliverable secrets in the list, edit prose without the value, split the long-reach code ([#31](https://github.com/krelinga/drydock/issues/31)) ([211a642](https://github.com/krelinga/drydock/commit/211a6423fc430f143c278e87c2f8f06a421cdcd1))
* verify installs against a private CA, lowercase the UI host, publish releases once verified ([#40](https://github.com/krelinga/drydock/issues/40)) ([ad812be](https://github.com/krelinga/drydock/commit/ad812be21a685c469c0647d505cb9d3a9cae5e8b))

## 0.1.0 (2026-10-04)


### Features

* add `drydock version` and `passwd --if-unset` for the installer ([8f793a1](https://github.com/krelinga/drydock/commit/8f793a1e9f05919d1da158102b2682ff257e8524))
* add an installer that installs and upgrades from a release ([6c0cf41](https://github.com/krelinga/drydock/commit/6c0cf41cd6dabfbcf339c755b6b5f37937aa6d9f))
