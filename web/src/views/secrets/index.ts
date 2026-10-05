// The secrets screens, one lazy chunk (frontend §3.1: secrets and logs are the
// two). Every /secrets route imports this module, so the chunk is fetched
// once, the first time any of them is visited.
export { default as SecretsListView } from './SecretsListView.vue'
export { default as SecretNewView } from './SecretNewView.vue'
export { default as SecretView } from './SecretView.vue'
