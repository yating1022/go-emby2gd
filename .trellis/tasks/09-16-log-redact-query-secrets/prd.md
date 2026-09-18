# 访问日志脱敏(查询参数中的密钥)

## Goal

访问日志原样记录完整 URI 含查询参数, 导致 Emby 的 X-Emby-Token/api_key、ge2o 自己的 api-secret 明文落盘(实测已发生 Google client_secret 泄漏)。对敏感查询参数做脱敏后再记录。

## Requirements

- TBD

## Acceptance Criteria

- [ ] TBD

## Notes

- Keep `prd.md` focused on requirements, constraints, and acceptance criteria.
- Lightweight tasks can remain PRD-only.
- For complex tasks, add `design.md` for technical design and `implement.md` for execution planning before `task.py start`.
