# Backend Development Guidelines

> Best practices for backend development in this project.

---

## Overview

This directory contains guidelines for backend development. Fill in each file with your project's specific conventions.

---

## Guidelines Index

| Guide | Description | Status |
|-------|-------------|--------|
| [Directory Structure](./directory-structure.md) | Package layout, regex route table, dependency direction, where new code lands | Filled |
| [Configuration & State Management](./database-guidelines.md) | No database — config loading, in-memory state patterns, global singletons | Filled |
| [Error Handling](./error-handling.md) | Dual error channels, checkErr exit, sentinel errors, API error formats | Filled |
| [Quality Guidelines](./quality-guidelines.md) | Forbidden/required patterns, testing conventions, review checklist | Filled |
| [Logging Guidelines](./logging-guidelines.md) | In-house logs package, 7 level functions, message conventions | Filled |
| [Response Cache Middleware](./response-cache.md) | `internal/web/cache` contracts: route whitelist, whole-body buffering, per-response cap, test landmines | Filled |
| [Outbound HTTP Proxying](./http-proxy.md) | Request/response header contracts, header canonicalisation, `Accept-Encoding: identity`, streaming rules | Filled |
| [GD Panel Direct Link](./gdrive-panel.md) | `internal/service/gdrive`: panel `/api/dl` contract, token cache margin invariant, retry codes, long-playback contract | Filled |
| [Agent Proxy Network](./agent-network.md) | master/agent proxy network: endpoints, signed URLs, margin chain, registry state file, install/release contract | Filled |

> Note: this project has no database. `database-guidelines.md` was repurposed as
> "Configuration & State Management" — all state is in-memory and rebuildable.

---

## How to Fill These Guidelines

For each guideline file:

1. Document your project's **actual conventions** (not ideals)
2. Include **code examples** from your codebase
3. List **forbidden patterns** and why
4. Add **common mistakes** your team has made

The goal is to help AI assistants and new team members understand how YOUR project works.

---

**Language**: All documentation should be written in **English**.
