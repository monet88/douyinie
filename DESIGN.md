---
name: Douyinie
description: Automated video localization pipeline and operator workspace for Chinese short-form video adaptation.
colors:
  primary: "#e7ff57"
  primary-ink: "#111400"
  neutral-bg: "#0b0d10"
  surface: "#111418"
  surface-raised: "#171b20"
  surface-soft: "#0f1216"
  border: "#252a31"
  border-strong: "#353c45"
  text: "#f1f4f7"
  text-soft: "#a7afb9"
  text-dim: "#8e99a4"
  info: "#72a8ff"
  success: "#63d69b"
  warning: "#f1b84b"
  danger: "#ff6b6b"
typography:
  display:
    fontFamily: "Inter, ui-sans-serif, system-ui, sans-serif"
    fontSize: "clamp(26px, 2.8vw, 38px)"
    fontWeight: 700
    lineHeight: 1.05
    letterSpacing: "-0.045em"
  headline:
    fontFamily: "Inter, ui-sans-serif, system-ui, sans-serif"
    fontSize: "20px"
    fontWeight: 700
    lineHeight: 1.2
    letterSpacing: "-0.03em"
  title:
    fontFamily: "Inter, ui-sans-serif, system-ui, sans-serif"
    fontSize: "14px"
    fontWeight: 650
    lineHeight: 1.3
    letterSpacing: "-0.015em"
  body:
    fontFamily: "Inter, ui-sans-serif, system-ui, sans-serif"
    fontSize: "13.5px"
    fontWeight: 400
    lineHeight: 1.65
  label:
    fontFamily: "ui-monospace, SFMono-Regular, Consolas, monospace"
    fontSize: "11px"
    fontWeight: 650
    lineHeight: 1.2
    letterSpacing: "0.12em"
rounded:
  sm: "4px"
  md: "9px"
  lg: "14px"
  full: "999px"
spacing:
  xs: "4px"
  sm: "8px"
  md: "14px"
  lg: "24px"
  xl: "34px"
components:
  button-primary:
    backgroundColor: "{colors.primary}"
    textColor: "{colors.primary-ink}"
    rounded: "{rounded.md}"
    padding: "8px 13px"
  button-primary-hover:
    backgroundColor: "{colors.primary}"
  button-secondary:
    backgroundColor: "{colors.surface-raised}"
    textColor: "{colors.text-soft}"
    rounded: "{rounded.md}"
    padding: "8px 13px"
  button-danger:
    backgroundColor: "rgba(255, 107, 107, 0.07)"
    textColor: "{colors.danger}"
    rounded: "{rounded.md}"
    padding: "8px 13px"
  input:
    backgroundColor: "{colors.surface-soft}"
    textColor: "{colors.text}"
    rounded: "{rounded.md}"
    padding: "10px 12px"
  status-pill:
    backgroundColor: "{colors.surface-raised}"
    textColor: "{colors.text-soft}"
    rounded: "{rounded.full}"
    padding: "3px 7px"
---

# Design System: Douyinie

## Overview

**Creative North Star: "The Cyberpunk Master Control Room"**

Douyinie's design system embodies the precision, focus, and atmosphere of a high-end digital mastering console. Built for a solo operator running a mission-critical localization pipeline on local GPU hardware, the interface discards frivolous decoration and marketing fluff in favor of high-density telemetry, razor-sharp visual hierarchy, and tactile direct manipulation.

The environment is cast in deep obsidian tones (`#0b0d10` through `#171b20`), creating an immersive darkroom experience that reduces ocular fatigue during intensive review sessions. Against this subdued architectural foundation, electric acid lime (`#e7ff57`) functions as high-voltage ocular guidance—drawing immediate focus to active stages, the timeline playhead, and decisive operator actions.

Every component is engineered for rapid keyboard/mouse triage: tight monospace telemetry labels, compact 1px borders, immediate hover states, and direct-manipulation bounding boxes that lock cleanly onto video coordinates.

**Key Characteristics:**
- **High-Density Telemetry:** Screen space is allocated to data—waveforms, tracks, bounding box dimensions, and confidence scores—not wasteful padding.
- **Tonal Layering:** Depth is communicated via deliberate background steps (`#0b0d10` -> `#111418` -> `#171b20`) and crisp 1px borders rather than noisy dropshadows.
- **Electric Accent Discipline:** Neon yellow-green is strictly rationed; rarity gives it commanding visual gravity.
- **Tactile Direct Manipulation:** Video overlay handles feature outward-expanding hit zones and real-time ghost bounding boxes for fail-closed coordinate editing.

## Colors

The palette balances deep, low-luminance obsidian surfaces with a focused telemetry spectrum and a single piercing neon accent.

### Primary
- **Electric Acid Lime** (`#e7ff57`): The system's primary beacon. Used for active navigation indicators, primary CTA buttons, timeline playhead needles, active bounding boxes, and current step indicators.
- **Charcoal Ink** (`#111400`): Deep acid-saturated black used exclusively for high-contrast typography resting atop the electric acid lime accent.

### Secondary (Terminal Telemetry Quad)
- **Cerulean Speech** (`#72a8ff`): Represents speech understanding, ASR alignment, active run pulse animations, and informational badges.
- **Jade Mint Pass** (`#63d69b`): Indicates completed stages, passing automated QA gates, healthy runtime states, and active execution slots.
- **Amber Exception** (`#f1b84b`): Flags review-required stages, OCR confidence warnings, duration overruns, and paused jobs.
- **Coral Blocker** (`#ff6b6b`): Designates pipeline errors, interrupted stages, geometrical overlap violations, and destructive actions.

### Neutral
- **Dark Void Base** (`#0b0d10`): The root background canvas.
- **Surface Soft** (`#0f1216`): Recessed containers, form inputs, timeline track basins, and inactive step indicators.
- **Surface Container** (`#111418`): Standard panel backgrounds, cards, and data table containers.
- **Surface Raised** (`#171b20`): Elevated active rows, segmented control selections, and secondary button backgrounds.
- **Border Hairline** (`#252a31`): Default 1px structural dividing lines across panels, tables, and nav items.
- **Border Strong** (`#353c45`): Focused input borders, hover states, and emphasized separators.
- **High-Contrast Text** (`#f1f4f7`): Primary headings, titles, active labels, and critical values.
- **Secondary Text** (`#a7afb9`): Lead paragraphs, body descriptions, and table values.
- **Muted Monospace Text** (`#8e99a4`): Timestamps, stage keys, run IDs, and metadata labels.

### Named Rules
**The High-Voltage Accent Rule.** The primary neon accent (`#e7ff57`) is reserved for ≤5% of any given screen surface, drawing instant focus to critical milestones (Active run, playhead, primary action, active selection); rarity creates its visual gravity.

## Typography

**Display & Body Font:** `Inter, ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif`  
**Label & Telemetry Font:** `ui-monospace, SFMono-Regular, Consolas, monospace`

**Character:** Clean, neutral sans-serif typography handles human-readable copy and structural navigation, while dense, tabular monospace typography anchors all metrics, timestamps, coordinate readouts, and runtime logs.

### Hierarchy
- **Display** (Bold 700, `clamp(26px, 2.8vw, 38px)`, line-height 1.05, letter-spacing -0.045em): Hero section titles and view headers.
- **Headline** (Bold 700, `20px`, line-height 1.2, letter-spacing -0.03em): Topbar titles and major view headings.
- **Title** (Semi-Bold 650, `14px`, line-height 1.3, letter-spacing -0.015em): Panel headings and modal titles.
- **Body** (Regular 400, `13.5px`, line-height 1.65): Explanatory copy, lead notes, and operational instructions (max line length ~75ch).
- **Label / Eyebrow** (Semi-Bold 650, `11px`, line-height 1.2, letter-spacing 0.12em, uppercase, monospace): Eyebrows, step indicators, table headers, and telemetry readouts.

### Named Rules
**The Telemetry Monospace Rule.** All numeric metrics, timestamps, stage IDs, CAS hashes, and coordinate readouts must be rendered in monospace font to ensure tabular alignment and immediate mechanical scanning.

## Layout

The workspace utilizes a fixed, full-viewport grid tailored for desktop workstation monitors:
- **App Shell:** Two-column grid with a sticky sidebar (248px width) and an expansive fluid workspace (`minmax(0, 1fr)`).
- **Topbar:** Sticky 84px header with subtle glassmorphic blur (`backdrop-filter: blur(14px)`), displaying real-time active run context, execution status, and posture pills.
- **Workflow Stepper:** Horizontally scrollable sequential pipeline tracker driven strictly by persisted RuntimeHost stage states.
- **Three-Zone Review Workspace:** The centerpiece inspection interface composed of:
  - *Zone 1 (Context & Speakers):* `minmax(250px, .72fr)` for speaker cards and metadata.
  - *Zone 2 (Video Player & Observational Timeline):* `minmax(420px, 1.48fr)` housing the 16:9 / 9:16 letterboxed video player, direct-manipulation overlay, and multi-track synchronized timeline.
  - *Zone 3 (Inspector & Editor Drawer):* `minmax(320px, 1fr)` featuring exception triage tabs, transcript rows, and fail-closed region editing forms.
- **Responsive Adaptations:**
  - `≤1200px`: Compact three-zone distribution.
  - `≤1050px`: Collapses sidebar to an icon rail (78px), stacks dashboard grids, and splits review workspace into a 2-column layout with Zone 1 spanning the bottom.
  - `≤760px`: Full single-column fluid layout with horizontal scrolling navigation.

## Elevation & Depth

Douyinie relies on **Tonal Stratification** rather than heavy shadows to convey architectural depth. The eye perceives hierarchy through stepped surface lightness:
1. Canvas Ground: `#0b0d10` (Dark Void)
2. Recessed / Inset Tracks: `#0f1216` (Surface Soft)
3. Standard Panels: `#111418` (Surface Container)
4. Elevated Cards / Modals: `#171b20` (Surface Raised)

### Shadow Vocabulary
- **Ambient Deep Glow** (`box-shadow: 0 20px 60px rgba(0, 0, 0, .28)`): Applied to elevated panels, principle cards, and toast notifications to lift them off the background canvas.
- **Neon Ring Pulse** (`box-shadow: 0 0 0 3px rgba(114, 168, 255, .18)`): Dynamic ring indicating active processing loops.

### Named Rules
**The Flat-By-Default Rule.** Surfaces remain strictly flat at rest. Dropshadows and glow rings appear solely as functional state indicators (active run pulse, elevated toast notification, or active drag gesture).

## Shapes

- **Radius Scale:**
  - `sm` (4px): Inner tags, timeline blocks, playhead handles, and direct manipulation drag handles.
  - `md` (9px): Interactive buttons, text inputs, form select fields, and speaker cards.
  - `lg` (14px): Major workstation panels, video wrapper containers, and metric cards.
  - `full` (999px): Status pills, telemetry badges, and playback indicator rings.
- **Form Language:** Clean, sharp geometric rectangles with subtle border radius softening to ensure high data density without feeling raw or unstyled.

## Components

### Buttons
- **Shape:** 9px radius (`rounded.md`), min-height 36px.
- **Primary:** Electric Acid Lime background (`#e7ff57`), Charcoal Ink text (`#111400`), 1px solid `#e7ff57`. Hover: `translateY(-1px)`.
- **Secondary:** Surface Raised background (`#171b20`), Soft text (`#a7afb9`), 1px solid Border Strong (`#353c45`). Hover: Border `#555f6b`, text `#f1f4f7`.
- **Danger:** Subdued red tint background (`rgba(255, 107, 107, .07)`), Coral Red text (`#ff6b6b`), 1px solid `rgba(255, 107, 107, .28)`.

### Form Fields & Inputs
- **Style:** Surface Soft background (`#0f1216`), 1px solid Border (`#252a31`), 9px radius (`rounded.md`), padding `10px 12px`.
- **Focus:** Border shifts to Electric Acid Lime (`#e7ff57`), background warms to `#12161a`, outline 2px solid `#e7ff57` with 2px offset.

### Status Pills
- **Style:** Compact pill (`rounded.full`), padding `3px 7px`, font: 650 10.5px/1 monospace, uppercase.
- **Variants:**
  - `running` / `queued`: Cerulean text (`#72a8ff`), border `rgba(114,168,255,.3)`, bg `rgba(114,168,255,.06)`.
  - `completed` / `pass`: Jade Mint text (`#63d69b`), border `rgba(99,214,155,.3)`, bg `rgba(99,214,155,.06)`.
  - `warning` / `review_required`: Warm Amber text (`#f1b84b`), border `rgba(241,184,75,.3)`, bg `rgba(241,184,75,.06)`.
  - `failed` / `blocker`: Coral Red text (`#ff6b6b`), border `rgba(255,107,107,.3)`, bg `rgba(255,107,107,.06)`.

### Observational Timeline
- **Components:** Rulers with 1-second ticks, synchronized playhead needle (`#e7ff57` with drop triangle handle), multi-track rows (Speech, Visual Text, Review Exceptions), and color-coded interactive blocks.
- **Interactivity:** Scrubbing anywhere updates video playback immediately; clicking a block navigates to its transcript or region editor.

### Direct-Manipulation Video Overlay
- **Bounding Box (`.region-box`):** 1.5px solid `#e7ff57`, background `rgba(231,255,87,.12)`, 2px radius, cursor `move`.
- **Ghost Box (`.region-ghost`):** 1px dashed `rgba(255,255,255,.5)` displaying original unedited bounding geometry.
- **Resize Handles (`.region-handle`):** 8-directional outward-expanding hit zones (11px × 11px) ensuring handles never overlap or block interior click-to-drag actions.

## Do's and Don'ts

### Do:
- **Do** maintain high data density; prioritize tables, timelines, and metric cards over decorative blank whitespace.
- **Do** render all timestamps, dimensions, stage IDs, and hashes in monospace font (`ui-monospace`).
- **Do** reserve the primary neon accent (`#e7ff57`) strictly for the active playhead, selected item, and primary call-to-action.
- **Do** position overlay bounding boxes relative to canonical video coordinates to prevent display-scaling distortion.
- **Do** display fail-closed review exception reasons clearly with Coral Red or Amber callout boxes.

### Don't:
- **Don't** use neon accent colors for general text or background cards; it causes extreme ocular fatigue.
- **Don't** use multi-colored gradients or decorative drop shadows on data tables or forms.
- **Don't** invent artificial rounded pill shapes for primary content containers; keep card corners at 9px to 14px.
- **Don't** introduce light mode themes; the workstation is intentionally hardcoded to dark obsidian mode (`color-scheme: dark`).
