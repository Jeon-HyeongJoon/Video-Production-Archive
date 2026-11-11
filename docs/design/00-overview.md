# 00. 탑다운 설계 개요

## 1. 설계 목표

최상위 목표는 영상 처리 속도의 극대화가 아니라 다음 세 가지다.

1. 촬영 원본과 처리 이력을 잃지 않는다.
2. Archivist가 없어도 사람이 같은 업무 기능을 계속 사용할 수 있다.
3. 32GB Windows PC 한 대에서 아카이브 작업 공간의 응답성을 유지하며 무거운 작업을 유휴 시간에 처리한다.

## 2. 설계 계층

```text
L0 업무 성과
  원본 보존 / 검색 가능 기록 / 반복 작업 자동화 / 사람 폴백
        ↓
L1 시스템 컨텍스트
  담당자 / 관리자 / 외부 Archivist / AI Provider / 외장 매체
        ↓
L2 실행 컨테이너
  Archive Control / Archive Media CLI·Runner / Archivist Host / SQLite / File Storage / OS Secret Store
        ↓
L3 애플리케이션 컴포넌트
  Command Bus / Workflow / Scheduler / Archivist Gateway / Domain Services
        ↓
L4 도메인 규칙
  상태 전이 / 불변성 / 승인 / 자원·장치 lock / 폴백 정책
        ↓
L5 Adapter
  HTTP·SSE / MCP / Runtime Backend / SQLite / FFmpeg / Windows Device / Provider API
```

의존성은 항상 아래 방향에서 Core 쪽으로 향한다. Domain Core는 HTTP, MCP, SQLite, FFmpeg, Windows API 또는 특정 AI SDK를 import하지 않는다.

## 3. 물리 구조와 논리 구조

초기에는 한 PC에서 실행하지만 하나의 프로세스에 모두 넣지 않는다.

```text
Windows PC
├── archive-control.exe
│   ├── Archive Workspace assets
│   ├── HTTP/SSE API
│   ├── MCP Adapter
│   ├── Archivist Gateway
│   ├── Workflow/Scheduler
│   └── SQLite 단일 writer
├── archive-media.exe
│   ├── 독립 MediaOperation CLI
│   ├── FFmpeg/ffprobe/STT·QC·카탈로그
│   └── 선택형 runner: Step lease·자원 관측·결과 보고 Adapter
├── archivist-host.exe
│   ├── ArchivistTurn lease/event client
│   ├── loopback MCP proxy
│   └── Embedded Core 또는 선택형 Orca CLI backend
├── OS Secret Store
├── Internal SSD: DB, logs
├── Scratch SSD: temporary outputs
└── External media: source/archive copies
```

macOS의 Control·Archivist 통합 배치에서는 같은 PC에 `archive-control`, `archivist-host`, `archive-archivist`를 둔다. `archive-archivist`는 Keychain OAuth Profile의 ChatGPT Responses 또는 같은 Host의 loopback 자체 LLM 중 하나를 CLI ID로 고정하고 한 Turn과 제한된 Archive MCP 업무 도구만 실행한다.

Worker가 다른 PC로 이동해도 Domain 계약과 Control API는 바뀌지 않는다.

단일 파일 처리에서는 `archive-media`만 실행하며 Control·대시보드·DB·Worker 등록을 요구하지 않는다. 여러 PC 큐 관리가 필요할 때만 같은 실행 파일의 `runner` 하위 명령을 상주시킨다. CLI와 runner는 같은 MediaOperation Port를 사용한다. `APP-009~010`

## 4. 설계별 소유권

| 설계 | 소유하는 결정 | 대표 요구사항 |
|---|---|---|
| Command / Event Contract | 권한 선판정, 동시성, 중복 방지, 명령 기록과 Event | CMD-001~008, PER-001 |
| Workflow Engine | DAG, StepState, ExecutionLease, fencing, 재시도, Approval | WFL-001~011 |
| Resource Scheduler | 유휴·메모리·scratch·동시성·디스크 | RES-001~008 |
| Dispatch Coordinator | 영속 ready queue, aging·Job 공정성, Worker 자원 판정과 원자 예약 | DSP-001~003 |
| Template | 영상 유형별 StepDefinition과 gate | PIP-001~005 |
| Archive Domain | 원본·파생본·manifest·확정·DB 없는 복원 | ARC-001~007 |
| Subprocess Boundary | 인수 배열, 임시 출력, publish 복구 | EXE-001~003 |
| Notification | 저장 Event replay와 HTTP/SSE 재연결 | NTF-001~002 |
| Device Management (planned) | 볼륨 lock, 제거, 포맷의 후속 장치 경계 | DEV-001~005 |
| Provider Account Router (planned) | 외부 Provider 계정, 라우팅, 폴백, 도구 안전, 비밀 제거 | AGT-001~015 |
| Archivist Host | 로컬 CLI 세션, turn 전달·재개, cursor event와 fencing | LRH-001~009 |
| MCP Adapter | 업무 의미 도구, Tool Call 멱등성과 Command 상관관계 | MCP-001~002 |
| Network Access | 내부 CIDR, v1 무로그인 접근, 후속 인증 경계, MCP 노출, CSRF | NET-001~007 |
| Authorization | 역할과 파괴적 작업 승인 | SEC-001~004 |
| Boot Configuration | 무관리 UI, 시작 설정, listen·파괴적 기능 정책, Secret reference | CFG-001~005 |
| Worker Fleet | 사전 등록, service 인증, 자원 snapshot heartbeat, 배정·실행 정산 | WRK-001~009 |
| Cancellation | 미시작 취소, 실행 중 lease fence, Worker stop·정리 확인, 완료 경합과 PARTIAL·UNCERTAIN 조정 | CAN-001~005 |
| Archive Workspace | 사람 직접 실행과 Archivist 도구 호출의 공통 업무 문맥, ActionCard, Archive Workspace 상태 투영 | COL-001~002, DSH-001~004 |
| MediaOperation 직접 실행 | Control 없이 단일 미디어 기능 실행, 관리 Runner와 같은 실행 경계 공유 | APP-009~010 |
| Application Capabilities (planned) | 모든 영속 업무 기능의 공통 Registry와 직접 실행 Port | APP-001 |
| Observability | 테스트의 구조화 증적. 모든 운영 Capability의 공통 Sink는 planned | TST-001, OBS-001(planned) |

도메인 명령과 이벤트의 시간 순서, 일관성 경계 후보와 아직 결정되지 않은 핫스팟은 [이벤트 스토밍](08-event-storming.md)에 정리한다. 핫스팟을 결정한 뒤 요구사항 카탈로그로 내린다.

Archivist Host의 세션·재시작·도구 경계와 Orca 최소 활용 결정은 [11. 로컬 CLI Archivist Host 설계](11-archivist-host.md)가 소유한다. Orca 전체 앱은 운영 필수 구성요소가 아니며 Orca CLI backend는 선택형 Adapter다.

취소, transaction rollback과 완료된 효과의 보상은 같은 동작으로 취급하지 않는다. 미시작·실행 중 Worker 취소와 완료 경합은 확정했고, 아직 결정되지 않은 외부 효과 조정·업무 보상은 [12. 취소와 보상 설계 제안](12-cancellation-compensation-proposal.md)에서 검토한다.

## 5. 현재 기준선의 완료 정의

현재 설계 기준선은 다음 조건을 모두 만족할 때 완료다.

- 각 구현 요구사항이 `requirements/catalog.json`에 원자적인 Given/When/Then으로 기록됨
- 각 `implemented` 요구사항에 정확히 하나의 Go 테스트 함수가 연결됨
- Adapter나 실장비가 필요한 미구현 계약은 테스트를 허위 연결하지 않고 `planned`로 표시됨
- 테스트가 실제 Domain Core의 결과 또는 상태 변화를 검증함
- `go run ./cmd/reqcheck` 성공
- `go test ./...` 성공
- 외부 실행기나 DB가 없어도 Core 테스트가 결정적으로 실행됨

## 6. 현재 수직 기준선과 다음 순서

현재 실제 제품 경로는 `논리 Volume 관찰 → 사람 선택 → ArchiveRun → Worker 검사·분류 → 렌더·QC → layout v3 compact 보관·ArchiveCatalog → 별도 STT → 패키지 리비전`까지 조립되어 있다. 단일 미디어 기능은 `archive-media` CLI로 Control 없이 실행하며 관리 Runner와 같은 MediaOperation을 사용한다. 사람·Archivist 협업은 Session·Turn·Tool Call·ActionCard·SYSTEM 재호출을 Control 원장에 보존한다.

현재 기능·경계·영속 객체·후속 작업은 [15. 현재 구현 기준선](15-current-baseline.md)에서 한 번에 확인한다. 다음 구현 순서는 다음과 같다.

1. 일반 Template Job의 실제 StepExecutionSpec 조립 또는 v1 화면 노출 범위 재결정
2. STT 게이트와 ArchiveRun 진행 읽기 모델 단일화
3. ArchiveRun 기간의 아카이브 Storage 쓰기 예약 `ARC-010`
4. 패키지 복원·아카이브 확정 `ARC-007`, `ARC-012`
5. Application에서 구체 Adapter를 직접 참조하는 Worker 조립 경계 이동
6. 실제 네트워크 Storage·한국어 STT·로컬 CLI 계정 acceptance

모든 후속 슬라이스는 Storage를 논리 Volume 작업 공간으로 해석하고 PhysicalDevice를 별도 I/O 충돌 키로 보존한다. 미디어 직접 실행은 `APP-009~010`으로 구현됐으며, 모든 영속 업무를 공통 Capability Registry로 실행하는 `APP-001`과 공통 운영 Evidence Sink `OBS-001`은 planned다. 테스트 증적 자체는 `TST-001`이 소유한다.
