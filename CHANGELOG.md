# Changelog

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
