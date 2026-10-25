# 00. 탑다운 설계 개요

## 1. 설계 목표

최상위 목표는 영상 처리 속도의 극대화가 아니라 다음 세 가지다.

1. 촬영 원본과 처리 이력을 잃지 않는다.
2. 에이전트가 없어도 사람이 같은 업무 기능을 계속 사용할 수 있다.
3. 32GB Windows PC 한 대에서 대시보드 응답성을 유지하며 무거운 작업을 유휴 시간에 처리한다.

## 2. 설계 계층

```text
L0 업무 성과
  원본 보존 / 검색 가능 기록 / 반복 작업 자동화 / 사람 폴백
        ↓
L1 시스템 컨텍스트
  담당자 / 관리자 / 외부 Agent / AI Provider / 외장 매체
        ↓
L2 실행 컨테이너
  Control Service / Media Worker / SQLite / File Storage / OS Secret Store
        ↓
L3 애플리케이션 컴포넌트
  Command Bus / Workflow / Scheduler / Agent Gateway / Domain Services
        ↓
L4 도메인 규칙
  상태 전이 / 불변성 / 승인 / 자원·장치 lock / 폴백 정책
        ↓
L5 Adapter
  HTTP·SSE / MCP / SQLite / FFmpeg / Windows Device / Provider API
```

의존성은 항상 아래 방향에서 Core 쪽으로 향한다. Domain Core는 HTTP, MCP, SQLite, FFmpeg, Windows API 또는 특정 AI SDK를 import하지 않는다.

## 3. 물리 구조와 논리 구조

초기에는 한 PC에서 실행하지만 하나의 프로세스에 모두 넣지 않는다.

```text
Windows PC
├── video-archive-control.exe
│   ├── Dashboard assets
│   ├── HTTP/SSE API
│   ├── MCP Adapter
│   ├── Agent Gateway
│   ├── Workflow/Scheduler
│   └── SQLite 단일 writer
├── video-archive-worker.exe
│   ├── Step lease client
│   ├── FFmpeg/ffprobe/STT executor
│   ├── QC executor
│   └── Device Adapter
├── OS Secret Store
├── Internal SSD: DB, logs
├── Scratch SSD: temporary outputs
└── External media: source/archive copies
```

Worker가 다른 PC로 이동해도 Domain 계약과 Control API는 바뀌지 않는다.

## 4. 설계별 소유권

| 설계 | 소유하는 결정 | 대표 요구사항 |
|---|---|---|
| Command / Event Contract | 권한 선판정, 동시성, 중복 방지, 명령 기록과 Event | CMD-001~008, PER-001 |
| Workflow Engine | DAG, StepState, ExecutionLease, fencing, 재시도, Approval | WFL-001~011 |
| Resource Scheduler | 유휴·메모리·scratch·동시성·디스크 | RES-001~008 |
| Template | 영상 유형별 StepDefinition과 gate | PIP-001~005 |
| Archive Domain | 원본·파생본·manifest·확정·DB 없는 복원 | ARC-001~007 |
| Subprocess Boundary | 인수 배열, 임시 출력, publish 복구 | EXE-001~003 |
| Notification | 저장 Event replay와 HTTP/SSE 재연결 | NTF-001~002 |
| Device Management | 볼륨 lock, 제거, 포맷 | DEV-001~005 |
| Agent Gateway | 계정, 라우팅, 폴백, 도구 안전, 비밀 제거 | AGT-001~013 |
| Network Access | 내부 CIDR, 선택형 인증, MCP 노출, CSRF | NET-001~007 |
| Authorization | 역할과 파괴적 작업 승인 | SEC-001~004 |
| Boot Configuration | 무관리 UI, 시작 설정, listen·파괴적 기능 정책, Secret reference | CFG-001~005 |
| Worker Fleet | 사전 등록, service 인증, heartbeat, 배정 | WRK-001~004 |

도메인 명령과 이벤트의 시간 순서, 일관성 경계 후보와 아직 결정되지 않은 핫스팟은 [이벤트 스토밍](08-event-storming.md)에 정리한다. 핫스팟을 결정한 뒤 요구사항 카탈로그로 내린다.

## 5. 현재 기준선의 완료 정의

현재 설계 기준선은 다음 조건을 모두 만족할 때 완료다.

- 각 구현 요구사항이 `requirements/catalog.json`에 원자적인 Given/When/Then으로 기록됨
- 각 `implemented` 요구사항에 정확히 하나의 Go 테스트 함수가 연결됨
- Adapter나 실장비가 필요한 미구현 계약은 테스트를 허위 연결하지 않고 `planned`로 표시됨
- 테스트가 실제 Domain Core의 결과 또는 상태 변화를 검증함
- `go run ./cmd/reqcheck` 성공
- `go test ./...` 성공
- 외부 실행기나 DB가 없어도 Core 테스트가 결정적으로 실행됨

## 6. 다음 수직 슬라이스

다음 구현 순서는 Phase 이름보다 담당자가 끝까지 사용할 수 있는 작은 흐름을 우선한다.

1. `입고 등록 → ffprobe → proxy → QC → 검색` 수직 슬라이스
2. SQLite repository, migration, backup/restore
3. Worker lease/heartbeat와 subprocess 복구
4. HTTP/SSE Dashboard의 작업 목록과 즉시 실행
5. 아카이브 sidecar export/import
6. VJ와 STT Template
7. MCP와 Agent Gateway
8. Windows 장치/포맷 Adapter

각 슬라이스도 요구사항과 테스트를 먼저 추가하며, 외부 프로그램이 필요한 검증은 fixture 기반 integration test와 실제 장비 acceptance test를 구분한다.
