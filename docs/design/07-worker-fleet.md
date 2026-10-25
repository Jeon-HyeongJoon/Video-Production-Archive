# 07. 다중 Worker 운영 설계

## 1. 목표

Control PC 한 대가 상태와 DB를 소유하고, 여러 Windows Worker PC가 렌더·STT·입고 Step을 수행한다. 초기 All-in-One 구성도 `worker-main` 하나가 같은 PC에 있는 특수한 경우로 취급한다.

```text
Dashboard ─┐
MCP/Agent ─┼→ Control Service → SQLite
           │       ↑
           │       ├── local worker-main
           │       ├── worker-edit-01
           │       └── worker-ingest-01
           └──── 상태·로그·승인
```

## 2. 사용자 인증과 Worker 인증 분리

개발 단계 Dashboard는 `auth_mode: disabled`이며 사용자별 계정이나 작업 소유자가 없다. Worker는 사람이 아니라 실행 결과를 기록하는 서비스 주체이므로 다음 값이 반드시 필요하다.

```text
worker_id
credential_ref
enabled
processing_kinds
allowed_storage_ids
heartbeat_ttl
```

사전 등록되지 않았거나 credential 검증에 실패한 Worker는 연결할 수 없다. `WRK-001`

credential 원문은 YAML에 저장하지 않는다. Control과 Worker의 로컬 Secret Store에 각각 reference를 두고 통신 Adapter가 실제 값을 가져온다. LAN을 통과하는 service credential은 TLS 또는 암호화된 overlay 안에서만 전송한다.

## 3. 시작 설정

Control 설정:

```yaml
workers:
  - id: worker-main
    enabled: true
    credential_ref: wincred://video-archive/workers/worker-main
    processing_kinds: [INSPECT, RENDER, CHECKSUM, DEVICE]
  - id: worker-stt-01
    enabled: true
    credential_ref: wincred://video-archive/workers/worker-stt-01
    processing_kinds: [INSPECT, STT]
```

Worker 설정:

```yaml
worker:
  id: worker-stt-01
  control_url: https://control.internal:8443
  credential_ref: wincred://video-archive/workers/worker-stt-01
  storage_roots:
    scratch: D:/VideoArchive/scratch
```

Worker 추가·비활성화·처리 종류 변경은 Control의 로컬 설정 파일을 수정하고 서비스를 재시작해 적용한다. 원격 enrollment 화면은 개발 단계에서 만들지 않는다. `CFG-001~003`

## 4. 연결과 상태

```text
Worker 시작
→ Control에 outbound connect + service credential
→ processing kind/tool version/storage/자원 snapshot 보고
→ 주기적 heartbeat
→ Step lease claim
→ progress/result 전송
```

heartbeat가 TTL을 넘으면 `OFFLINE`으로 간주하고 신규 lease를 주지 않는다. `WRK-002`

권장 상태:

```text
CONNECTING | ONLINE | BUSY | DRAINING | OFFLINE | DISABLED
```

`DRAINING`은 새 lease를 받지 않고 현재 작업만 끝내는 운영 상태다. Control 재시작 후 Worker는 다시 연결하고, 기존 Step은 lease 만료·idempotency 규칙으로 복구한다.

## 5. Worker 선택

Scheduler는 다음 순서로 후보를 거른다.

1. 설정에서 enabled이며 heartbeat가 유효함
2. Worker가 새 작업 수락 상태임
3. `RENDER`, `STT`, `DEVICE` 등 필요한 처리 종류 지원
4. 필요한 storage ID가 설정의 `allowed_storage_ids`와 heartbeat 접근 보고의 교집합에 있음
5. 특정 물리 장치 Step이면 해당 Worker에 pin하고 그 Worker가 offline일 때 다른 Worker로 우회하지 않음
6. 메모리·GPU·heavy slot 조건 충족
7. 현재 실행 중인 heavy 단계 수가 적은 Worker 우선

처리 종류와 storage 교집합 계약은 `WRK-003`, 물리 장치 pinning은 `WRK-004`가 검증한다.

## 6. 저장 위치와 데이터 이동

- USB/SD/HDD가 Worker A에 연결되면 장치 분석·복사 단계는 Worker A에 고정한다.
- 로컬 scratch artifact는 다른 Worker가 직접 볼 수 있다고 가정하지 않는다.
- 다음 단계가 다른 Worker에서 실행되어야 하면 먼저 등록된 shared storage 또는 명시적인 transfer 단계로 publish한다.
- NAS storage ID가 같아도 각 Worker heartbeat가 실제 접근 가능 상태를 보고해야 한다.
- SQLite 파일은 공유하지 않으며 모든 metadata write는 Control API를 통한다.

## 7. 장애와 중복 방지

- Worker 연결이 끊겨도 ExecutionLease TTL 전에는 같은 Step을 다른 Worker에 주지 않는다.
- TTL 후 실제 Artifact publish 여부와 command idempotency receipt를 먼저 확인한다.
- 재발급한 ExecutionLease는 더 큰 fencing token을 가지며 이전 Worker의 늦은 결과는 거부한다. `WFL-007~009`
- 완료되지 않은 단계만 재대기한다.
- 임시 결과는 Worker별 scratch namespace에 둔다.
- 동일 물리 볼륨 lock은 Worker가 달라도 Control에서 전역 관리한다.

## 8. 대시보드

인증 비활성 개발 모드에서도 Worker 상태는 읽기 화면으로 제공한다.

- worker ID와 hostname
- ONLINE/BUSY/OFFLINE 상태
- CPU, memory, GPU/VRAM, scratch 여유
- 처리 종류와 tool version
- 접근 가능한 storage와 연결 장치
- 실행 중 Step, 마지막 heartbeat와 오류

Worker 등록·credential 변경·삭제 버튼은 제공하지 않는다. 설정 파일을 바꾸고 Control Service를 재시작한다.
