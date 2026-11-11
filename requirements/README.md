# 요구사항과 실행 가능한 명세

## 1. 단일 기준

[`catalog.json`](catalog.json)이 제품 요구사항의 단일 기준이다. 각 항목은 다음을 가진다.

- 변경하지 않는 요구사항 ID와 소유 설계 영역
- 한 가지 관찰 가능한 업무 결과
- 구체적인 Given / When / Then
- `implemented` 또는 `planned` 상태
- `unit`, `integration`, `acceptance` 테스트 수준
- 구현된 경우 정확한 테스트 파일과 함수 이름

`implemented` 요구사항의 테스트 함수 바로 위에는 `// Requirement: XXX-000` 주석을 둔다. [`cmd/reqcheck`](../cmd/reqcheck/main.go)는 카탈로그와 Go AST를 비교하여 구현 요구사항과 테스트가 정확히 1:1인지 확인한다.

[`semantic-lock.json`](semantic-lock.json)은 ID별 Design/Statement/Given/When/Then/status/level의 SHA-256 지문이다. 테스트 파일 이동은 지문을 바꾸지 않지만 업무 의미 변경은 검사를 실패시킨다. 의미 변경을 검토한 뒤에만 `make requirements-lock`으로 갱신한다.

## 2. 상태와 테스트 수준

| 값 | 뜻 | 카탈로그 규칙 |
|---|---|---|
| `implemented` | 현재 저장소에서 동작을 관찰할 수 있다 | 테스트 파일·함수와 정확히 한 개의 주석 테스트가 필수 |
| `planned` | 계약은 정했지만 필요한 Adapter나 실장비가 아직 없다 | 테스트 참조와 주석 테스트를 두지 않음 |
| `unit` | Core의 규칙을 메모리에서 검증한다 | 외부 서비스 없이 반복 실행 가능 |
| `integration` | filesystem, SQLite, HTTP 같은 Adapter 경계를 검증한다 | 실제 경계 또는 충실한 테스트 인스턴스 사용 |
| `acceptance` | 운영체제·GPU·이동식 장치 등 실환경을 검증한다 | 격리된 전용 장비와 명시적 실행 절차 필요 |

`planned`는 테스트를 생략해도 된다는 뜻이 아니다. Adapter 또는 장비 준비 전의 정직한 상태 표시이며, 구현을 시작할 때 테스트 참조를 추가하고 `implemented`로 전환한다.

## 3. 원자성 규칙

- 한 요구사항은 한 가지 실패 이유 또는 한 가지 업무 결과만 소유한다.
- 같은 결과의 경계값 여러 개는 하나의 table-driven 테스트에서 검증할 수 있다.
- 서로 독립적으로 바뀔 수 있는 결과는 요구사항과 테스트를 나눈다.
- private 구현이 아니라 상태, 오류, 명령 기록, 도메인 이벤트 또는 아티팩트를 관찰한다.
- mock 호출 횟수는 중복 실행 방지처럼 횟수 자체가 업무 결과일 때만 검증한다.
- 이벤트 이름만 확인하지 않는다. 수락 명령은 감사 문맥과 전후 리비전을, 거부 명령은 오류와 이벤트 부재를 함께 검증한다.
- 실제 장비가 필요한 요구사항도 고유 ID와 단일 acceptance 시나리오를 가진다.
- 커버리지 수치를 올리거나 우연한 내부 구현을 고정하기 위한 테스트는 추가하지 않는다. 테스트는 카탈로그의 관찰 가능한 결과만 검증한다.
- 실제 기술 경계의 제한이나 더 나은 설계가 확인되면 Adapter를 억지로 기존 문구에 맞추지 않는다. 근거를 검토한 뒤 소유 설계, 요구사항, semantic lock과 1:1 테스트를 같은 변경에서 함께 수정한다.
- 테스트 검증은 CI 결과만 완료 판정에 사용한다. CI runner는 코드 리비전·환경·테스트 이름·요구사항 ID·수준·결과·시간과 산출물 참조를 기계 판독 가능한 artifact로 누적한다. `TST-001`
- Core unit test에 운영 모니터링 의존성을 주입하지 않는다. runner가 모든 테스트 결과를 기록한다. 모든 Application Capability를 공통 Evidence Sink로 감싸는 운영 증적 경계는 아직 제품 runtime에 연결되지 않은 `OBS-001` planned 계약이다.

## 4. 이벤트 스토밍에서 내려온 계약

[`08-event-storming.md`](../docs/design/08-event-storming.md)의 명령 → 판정 → 이벤트 → 정책 흐름을 다음 테스트 계약으로 내렸다.

| 영역 | 요구사항 | 고정한 핵심 규칙 |
|---|---|---|
| Command와 Event | `CMD-001~010`, `PER-001~002` | 권한 선판정, 의미 기반 멱등성, 결정적 실패 receipt, Unit of Work, SQLite 원자성과 process-wide 연결 수명주기 |
| Step 실행 | `WFL-001~013` | 단일 상태 전이표, 최신 Approval 결정, ExecutionLease 배타성, fencing, 게시 후 복구, 실패 분류 |
| Dispatch와 자원 | `RES-001~008`, `DSP-001~004` | 영속 ready queue, aging·Job 공정성, Worker 자원 판정, 활성 예약을 반영한 큐 admission·lease |
| 실행 경계 | `EXE-001~002`, `EXE-003`(planned) | shell 없는 인수 배열, 임시 출력 검증 뒤 게시. 게시 뒤 결과 보고 전 중단 복구는 후속 통합 경계 |
| 아카이브 | `ARC-001~020` | 원본 불변, provenance, 결정적 manifest, 보관됨·확정됨 분리, DB 없는 복원, 아카이브 Storage 선택·확정, flat 결과 폴더·카탈로그 대조와 안전 여유를 포함한 용량 admission |
| 내용 검수 | `REV-001` | 대상 리비전의 추가 전용 ContentReview와 최신 결정 투영 |
| 알림 | `NTF-001~002` | 저장 이벤트 replay와 일시 metric 분리, SSE 재연결 |
| Archivist | `LRH-001~008`, `LRH-010~013`, `LRH-016`, `LRH-018~021`, `MCP-001~008`, `AGT-016~021`; `LRH-009`, `LRH-014~015`, `LRH-017`, `AGT-001~015`(planned) | 로컬 CLI Archivist 수직 경로, Codex app-server native thread 재개·승인 없는 터미널·파일·Skill·보조 Agent·Archive MCP bridge, 선택·작업 참조의 정상적인 미고정 조회, CLI 실행 중 독립 Host heartbeat, Windows 대화형 사용자 Codex 로그인 경계와 macOS의 단일 ChatGPT 인증 Profile·loopback 자체 LLM·제한 도구 루프. 실계정 전체 협업·실제 자체 LLM acceptance와 다계정 Provider Router·폴백·계정 health는 아직 제품 경로에 연결하지 않음 |
| 장치와 Worker | `DEV-006~013`, `WRK-001~015`, `CFG-008~014`, `CAN-001~005`; `DEV-001~005`(planned) | Worker별 논리 드라이브·Storage, 점진적 폴더 inventory, lease 갱신, durable 완료 조정, typed 오류, 취소 보상, 로컬 media toolchain과 단일 Windows runtime root. 장치 포맷·eject lock은 후속 경계 |
| Archive Intake | `ORG-001~015`, `MED-005~008`, `RND-004~006`, `QCT-003~004`, `AUD-004`, `TRN-005~007`, `PIP-007~009`, `APP-002~007`, `ARC-009~020`, `DSH-006~014`, `DSH-016`, `DSH-018~033` | ArchiveRun, 사람의 원본·아카이브·폴더 선택, Worker 검사, HOLD 분류 뒤 동일 실행 재개, 1단계 보관, 별도 2단계 STT, 전사 품질 확인 필요, 날짜·원본 기반 결과명, ArchiveRun 상위 작업과 하위 Job 상세 이동·기존 저장 데이터 복원·Job별 영상 정보 지연 펼침, Worker·드라이브·Agent 연결 관리, 작업 관리의 독립 Job 생성, 보관됨·확정됨, versioned ArchivePackage layout과 ArchiveCatalog 대조, 안전 여유 용량 admission |
| 작업 공간·직접 실행 | `DEV-006`, `APP-009~010`; `APP-001`(planned) | Storage를 논리 Volume 작업 공간으로 해석하고 PhysicalDevice를 I/O 충돌 키로 분리. 미디어 처리는 CLI와 관리 Runner가 같은 MediaOperation을 직접 사용하며, 모든 영속 업무를 공통 Capability Registry로 실행하는 경계는 아직 운영 조립에 연결되지 않음 |
| 아카이브 작업 공간·증적 | `COL-001~004`, `DSH-001~017`, `DSH-019~040`, `CTL-007~008`, `TST-001`; `OBS-001`(planned) | 사람 직접 실행과 Archivist 도구 호출의 공통 업무 문맥, 이력을 보존하는 Agent Session retirement, 작업 도중 이관할 영속 작업과 드라이브 선택의 구조화된 참조, 예약 차감 Worker 자원 상태, 실패 결과와 후속 작업 인수를 분리한 미해결 관리 작업, 미완료 작업만 둔 워크스페이스 탐색과 완료 이력을 보존하는 작업 관리, 연결 관리·드라이브 상세와 문제 해결 위치 이동, 최근 작업 범위와 전체 드라이브 inventory를 분리한 안전 표시, ArchiveRun과 독립 Job의 진행 중·작업 큐·확인 필요·완료 구분과 새 독립 Job 생성, ArchiveRun 고정 범위·하위 Job·상위 실패 증적 projection과 기존 저장 데이터 복원, Job별 영속 영상 기술정보의 지연 펼침, 점진적 폴더 탐색과 범위 잠금, 사람이 읽는 아카이브 결과명·Package 상대경로와 실행을 넘어 유지되는 완료 내역, ArchiveCatalog 기록 시각, 선택 요청 카드 종결, 새 문제 알림과 낡은 화면 잠금, 기존 화면을 직접 대체하는 API 전용 component shell과 asset 재검증, 조작 화면과 golden 증적. 모든 Capability의 공통 운영 Evidence Sink 연결은 후속 경계 |
| Archivist Host | `LRH-001~008`, `LRH-010~013`, `LRH-016`, `LRH-018~021`, `MCP-001~008`, `CTL-005~006`, `CTL-008`, `AGT-016~021`, `CFG-015`; `LRH-009`, `LRH-014~015`, `LRH-017`(planned) | Host 인증·Turn lease·CLI 실행 중 독립 heartbeat·native CLI thread 재개·승인 없는 native 도구·Tool Call·ActionCard, 단계 완료 SYSTEM 재호출·업무 문맥, 정제된 진행 항목, 선택 범위의 HOLD 검토·단일 분류와 선택·작업 참조의 미고정 조회 MCP 경계 및 Windows 대화형 Codex 로그인·macOS ChatGPT·loopback 자체 LLM 로컬 루프 |

실제 DB transaction과 공유 연결 수명주기, HTTP 재연결은 각각 `PER-001~002`, `NTF-002`의 integration test로 검증한다. sidecar 복원과 게시 뒤 결과 보고 전 중단 조정은 아직 제품 수직 경로에 연결되지 않아 `ARC-007`, `EXE-003`을 planned로 유지한다. Windows junction 경계는 `ARC-008`의 전용 Windows acceptance test로 검증한다. 장치 lock·eject·format 경계는 `DEV-001~005`로 계획 상태를 유지한다.

## 5. 현재 기준선

- 전체 요구사항: 302개
- 구현됨: 262개, 주석 테스트 262개와 1:1 연결
- 구현된 integration 테스트: `ARC-006`, `ARC-013`, `ARC-015~020`, `PER-001~002`, `NTF-002`, `DSH-001~002`, `DSH-004~007`, `DSH-009~014`, `DSH-016~026`, `DSH-031~035`, `DSH-037~040`, `DEV-006~008`, `DEV-010`, `DEV-013`, `APP-002~004`, `APP-006`, `APP-008`, `TST-001`, `MED-002`, `MED-004`, `MED-006~008`, `INT-001`, `INT-003~004`, `RND-002`, `RND-005~006`, `QCT-002`, `QCT-004`, `CAT-002~003`, `AUD-002`, `AUD-004`, `TRN-002`, `TRN-004`, `TRN-006`, `SUB-002`, `SUM-003`, `REV-002`, `CFG-006~011`, `CTL-001~008`, `WRK-005~006`, `WRK-008~015`, `LRH-001`, `LRH-012~013`, `LRH-016`, `LRH-021`, `MCP-001`, `MCP-003~005`, `MCP-008`, `ORG-001`, `ORG-008~009`, `ORG-015`, `ARC-011`, `COL-001`, `COL-003~004`, `DSP-003`, `CAN-001~005`, `AGT-016~018`, `AGT-021`
- 구현된 실장비 acceptance 테스트: `ARC-008`, `ARC-014`, `MED-003`, `RND-003`, `AUD-003`, `LRH-010~011`, `LRH-020`, `APP-005`, `APP-007`, `CFG-014~015`
- 계획됨: 40개
- 계획된 unit 테스트: `DEV-001~004`, `AGT-001~015`
- 계획된 실장비 acceptance 테스트: `DEV-005`, `TRN-003`, `WRK-007`, `LRH-009`, `LRH-014~015`, `LRH-017`, `DEV-009`, `DEV-012`, `TRN-005`, `CFG-012~013`, `ORG-010`
- 계획된 integration 테스트: `APP-001`, `ARC-007`, `ARC-010`, `ARC-012`, `EXE-003`, `OBS-001`, `SUM-002`, `DSH-015`

정확한 문구와 테스트 이름은 항상 [`catalog.json`](catalog.json)에서 확인한다. 이 문서에는 변경 시 쉽게 낡는 전체 테스트 목록을 복제하지 않는다.

## 6. 실행

```bash
go run ./cmd/reqcheck
go test ./...
```

현재 정상 출력:

```text
traceability ok: 297 requirements (257 implemented, 40 planned), 257 one-to-one tests
```

## 7. 새 요구사항 추가 절차

1. 용어집과 이벤트 스토밍에서 명령, 이벤트, 실행자, 대상, 일관성 경계를 확인한다.
2. 소유 설계 문서와 Prefix를 선택한다.
3. 카탈로그에 원자적인 Given / When / Then, 상태와 테스트 수준을 기록한다.
4. Core로 검증할 수 있으면 예정 테스트 파일·함수를 기록하고 같은 ID의 실패 테스트를 작성한다.
5. Adapter를 구현할 때는 실제 filesystem, SQLite, HTTP 같은 경계를 사용하는 실패 테스트를 먼저 작성하고 요구사항에 필요한 최소 동작만 구현한다.
6. Adapter나 실장비가 아직 없으면 테스트를 허위 연결하지 않고 `planned`로 둔다.
7. 테스트 과정에서 기술 제한이나 더 나은 구조가 발견되면 소유 설계와 요구사항을 검토해 고친 뒤 같은 변경에서 테스트도 고친다.
8. 최소 구현 후 `reqcheck`와 전체 테스트를 통과시키고 `implemented`로 전환한다.
