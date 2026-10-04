<script setup lang="ts">
// The fleet-wide banner slot (frontend §6.6). One fault with one cause gets
// one message and one button here, above every card, rather than a button on
// each. Phase 5's identity store supplies the five Claude-login states; until
// then nothing is fleet-wide and the slot renders nothing.
import { computed } from 'vue'

interface Banner {
  tone: 'warn' | 'bad'
  title: string
  body: string
}

const banner = computed<Banner | null>(() => null)
</script>

<template>
  <div v-if="banner" class="msg" :class="banner.tone" role="status">
    <span class="glyph" aria-hidden="true">!</span>
    <div>
      <b>{{ banner.title }}</b>
      <p>{{ banner.body }}</p>
    </div>
  </div>
</template>
