# Archive

촬영 원본의 입고부터 파생 영상, 전사, 메타데이터, 아카이브와 검색 기록까지 관리하는 사무실용 영상 기록 시스템이다.

이 리포지토리는 `idea.md`의 방향을 탑다운 설계와 실행 가능한 도메인 계약으로 구체화한다. 현재 기준선은 외부 프로그램과 DB를 붙이기 전에 반드시 안정되어야 하는 핵심 규칙을 구현한다.

## 현재 구현 범위

- 공통 Command envelope, 권한 선판정, optimistic concurrency, 의미 기반 idempotency와 명령·Event journal 계약
- 영속 워크플로에 사용할 상태 전이, Approval 리비전, ExecutionLease fencing, 게시 결과 복구와 실패 분류
- 32GB 단일 PC용 유휴·메모리·scratch·heavy slot·물리 장치 예약 정책
- 행사 기록, VJ, 최종 편집본, 대용량 파일 입고 Template의 무순환·자원·처리 기능 선언
- 원본 불변성, 파생본 provenance, 결정적 manifest, 아카이브 확정 조건과 DB 없는 메타데이터 round-trip
- 외장 볼륨 lock과 포맷 사전조건
- Provider 계정 분리 저장, cooldown 복구·overload 격리, 안전한 Tool Call, 중복 없는 폴백과 허용 목록 로그
- shell을 쓰지 않는 subprocess 인수 계약과 검증 전 임시 출력 비공개
- 저장 Domain Event와 일시적 진행률·자원 알림을 분리한 재연결 feed
- 사람·Archivist 권한 정책
- 내부 CIDR allowlist, v1 무로그인 내부망 접근, 후속 인증 경계와 CSRF/Origin 방어
- 시작 설정 전용 관리 모델과 여러 작업 PC Worker registry·heartbeat·배정 정책
- 요구사항과 테스트의 자동 1:1 추적 검사
- 등록 Storage 입고·기술정보 분석·검증된 렌더·통합 QC·메타데이터 검색, timestamp 전사·SRT 자막과 리비전 요약 계약
- strict YAML boot 설정, 재시작 가능한 SQLite Runtime, CIDR 보호 아카이브 작업 공간과 조회 API를 제공하는 `archive-control`
- Control 없이 파일 처리만 실행하는 `archive-media` CLI와, 필요할 때 같은 실행 파일에서 credential 인증 heartbeat·Step claim을 수행하는 선택형 `runner`
- Worker·원본·아카이브·폴더 범위를 고정하는 ArchiveRun, 1단계 분류·렌더·QC·보관과 별도 2단계 STT·자막·텍스트·패키지 리비전·ArchiveCatalog
- 세션 제어 모드, 구조화된 드라이브 선택 참조, MCP 업무 도구, ActionCard와 단계 완료 SYSTEM 재호출을 잇는 Archivist 협업 흐름
- macOS Keychain에 보호된 ChatGPT Profile·stateless streaming Responses Adapter, loopback 자체 LLM Chat Completions Adapter와 Archive 업무 도구만 사용하는 bounded `archive-archivist` 루프

현재는 SQLite Command Unit of Work, Storage→논리 Volume 작업 공간 해석과 별도 PhysicalDevice I/O 충돌 키, 아카이브 sidecar export/import, 게시 장애 조정, Notification SSE 재연결, 독립 Capability 실행·증적·Archive Workspace 상태 투영과 CI 테스트 증적의 integration 계약까지 구현되어 있다. 등록 Storage 입고 카탈로그와 ffprobe·FFmpeg·whisper.cpp subprocess Adapter도 격리 fixture로 검증하며 FFmpeg speech audio 추출에서 timestamp Transcript 게시·최신 segment 검색, 검증된 SRT 자막, Archivist·사람 작성 리비전 요약 게시와 SQLite 내용 검수 복원까지 연결된다. `archive-control`은 boot 설정으로 같은 SQLite DB와 Storage topology를 열고 내부망 아카이브 작업 공간, 자산·전사·요약·검수·Job·Worker·Archivist Runtime 조회 API와 CSRF 보호 Job·Session·Turn 명령을 제공한다. `archive-media`는 같은 MediaOperation Adapter를 단독 CLI와 선택형 관리 Runner에서 함께 사용한다. 단독 실행은 Control·대시보드·SQLite·서비스 credential을 열지 않으며, `runner` 모드만 heartbeat, Step claim·결과와 실행 중 취소 stop·정리 결과를 전송한다. `archivist-host`는 허용된 로컬 CLI와 workspace만 실행하고 Control에 outbound heartbeat·Turn claim·event를 보내며 loopback MCP proxy를 제공한다. Control MCP Adapter는 versioned 업무 Capability만 같은 Command Bus에 연결한다. `archive-archivist`는 ChatGPT 계정 model 또는 같은 Host의 자체 LLM과 그 MCP proxy를 잇는 단일 Turn 루프다. Archivist Host native resume, Windows Device Adapter, VJ 편집 기능과 다계정 Provider Router는 아직 남아 있다. 로컬 CLI 경로는 전체 Orca 앱 대신 최소 Embedded Core를 기본으로 하고 Orca CLI는 선택형 backend로 둔다. 실제 Windows Credential Manager·장치·CLI와 운영 ffprobe·FFmpeg·whisper.cpp 실행은 장비 acceptance로 검증한다.

영속 Dispatch Queue의 aging·Job 공정성, 전체 자원 snapshot heartbeat와 ResourceReservation·ExecutionLease·fencing token의 SQLite 원자 확정이 구현되어 있다. Control은 heartbeat 직후 ready Step을 평가해 Worker 자원을 예약한다. Worker는 lease를 claim해 실행하고 결과를 정산하며, 실행 중 취소는 lease를 먼저 fence한 뒤 process 종료와 임시 출력 정리를 확인해야 예약을 해제한다.

## 문서 읽는 순서

1. [설계 인덱스](docs/design/00-overview.md)
2. [시스템 컨텍스트](docs/design/01-system-context.md)
3. [컨테이너와 경계](docs/design/02-containers.md)
4. [도메인과 워크플로](docs/design/03-domain-workflow.md)
5. [데이터와 아카이브](docs/design/04-data-archive.md)
6. [실행·자원·배포](docs/design/05-runtime-deployment.md)
7. [Archivist와 보안](docs/design/06-archivist-security.md)
8. [다중 Worker 운영](docs/design/07-worker-fleet.md)
9. [이벤트 스토밍](docs/design/08-event-storming.md)
10. [아카이브 작업 공간](docs/design/09-archive-workspace.md)
11. [도메인 기능 실행과 검증 증적](docs/design/10-capabilities-observability.md)
12. [로컬 CLI Archivist Host](docs/design/11-archivist-host.md)
13. [취소와 보상 설계 제안](docs/design/12-cancellation-compensation-proposal.md)
14. [Archive Intake](docs/design/13-archive-intake.md)
15. [아카이브 작업 공간 UX·UI 설계 기준](docs/design/14-archive-workspace-ux.md)
16. [요구사항 운영 규칙](requirements/README.md)
17. [기계 판독 요구사항 카탈로그](requirements/catalog.json)

초기 설정 예시는 [config.example.yaml](config.example.yaml)에 있다. 실제 credential은 이 파일에 쓰지 않고 Windows Credential Manager·DPAPI 또는 macOS Keychain reference만 기록한다.

## 검증

아카이브 작업 공간의 시각 계약은 실제 Chromium이 실행 전·1단계 진행·STT 가능·완료 네 장면의 고정 fixture를 렌더링한 장면별·운영체제별 PNG golden으로 검증한다. 처음 한 번 브라우저 의존성을 설치한 뒤 전체 검증을 실행한다.

```bash
make visual-deps
make verify
go vet ./...
```

의도적으로 화면 모양을 바꾼 경우 실제 이미지를 먼저 검토하고 golden을 명시적으로 갱신한다. 갱신 명령은 지금 실행 중인 운영체제의 `archive_workspace_<장면>_<OS>.golden.png`만 쓴다. Linux·Windows golden은 해당 운영체제에서 실행하거나 CI가 남긴 `<장면>_<OS>.actual.png`를 검토한 뒤 반영한다.

```bash
make archive-workspace-golden-update
go run ./cmd/reqcheck
go run ./cmd/ontocheck
go test ./...
```

`ontocheck`는 [`ontology/terms.json`](ontology/terms.json)의 용어 정의가 서로 모순되지 않는지, 금지된 코드 식별자가 쓰이지 않는지 검사한다. 한 단어가 두 용어를 가리키거나, 아직 결정되지 않은 다의어(`open`)가 결정된 용어와 겹치면 실패한다. 용어 결정 때문에 코드·요구사항·문서에서 바꿔야 할 것은 `follow_ups`에 기록하고, 코드 이름을 바꾼 뒤에는 옛 이름을 `forbidden.code`로 옮긴다.

## Archive Control 실행

[`config.example.yaml`](config.example.yaml)을 `config.yaml`로 복사하고 현재 PC의 listen 주소, 허용 CIDR, Storage identity와 root를 설정한다. credential 원문은 넣지 않고 OS Secret Store reference만 기록한다.

```bash
go run ./cmd/archive-control -config config.yaml -check-config
go run ./cmd/archive-control -config config.yaml
```

설정된 주소에서 아카이브 작업 공간과 `/api/v1`을 제공한다. 개발 PC에서 확인할 때도 wildcard listen 대신 loopback 주소와 `127.0.0.0/8`을 명시한다.

## 파일 처리 CLI와 선택형 Runner

[`media.example.yaml`](media.example.yaml)을 `media.yaml`로 복사해 Storage root와 로컬 도구 경로를 맞춘다. `archive-media operations`는 지원하는 모듈을 보여준다. `inspect`, `render`, `sequence`, `qc`, `package`, `audio`, `transcribe`, `subtitle`, `transcript-text`, `package-revise`, `catalog` 명령은 typed operation JSON 하나를 직접 실행하며 Control이나 아카이브 작업 공간을 시작하지 않는다.

```bash
go run ./cmd/archive-media operations
go run ./cmd/archive-media check -config media.yaml
go run ./cmd/archive-media render -config media.yaml -spec render.json
go run ./cmd/archive-media catalog -config media.yaml -spec catalog.json
```

각 명령은 stdout에 성공 여부, 결과 상대경로와 checksum을 JSON으로 반환한다. 입력 원본은 읽기 전용이며 출력은 임시 파일 검증 뒤 게시된다. 서로 다른 출력은 CLI와 Runner에서 동시에 실행할 수 있고, 같은 Storage 출력은 교차 프로세스 잠금으로 한 번에 하나만 실행한다.

여러 PC 큐·자원·취소·대시보드가 필요할 때만 같은 바이너리를 관리 Runner로 실행한다.

Control과 Worker PC의 OS Secret Store에 같은 service credential을 저장한다. 사용자 profile이 적재된 서비스 계정은 `wincred://` generic credential을 사용할 수 있다. profile 없는 Windows Service나 OpenSSH 배포는 `archive-secret -dpapi-reference dpapi://C:/절대/경로.dpapi`에 credential을 표준 입력으로 전달하고, blob 경로 ACL을 해당 서비스 계정과 `SYSTEM`으로 제한한다. credential 원문은 YAML이나 명령행에 넣지 않는다. 그 다음 예시를 `worker.yaml`로 복사해 Control URL과 접근 가능한 Storage ID를 맞춘다. 다른 PC의 Control에 연결할 때는 HTTPS URL이 필수다.

```bash
go run ./cmd/archive-media runner -config worker.yaml -check-config
go run ./cmd/archive-media runner -config worker.yaml
```

Worker가 연결되면 아카이브 작업 공간의 “Worker 연결” 상태 영역과 `/api/v1/workers`에서 ONLINE 상태를 확인할 수 있다.

## macOS 로컬 Archive Archivist

`archive-archivist`는 Archive Control과 함께 둔 `archivist-host`가 한 Turn씩 실행하는 Archivist 루프다. Control의 영속 Session·Turn을 그대로 사용하며 별도 대화 DB나 범용 shell을 만들지 않는다. 모델에는 Control MCP와 로컬 schema registry 양쪽에 모두 존재하는 Archive 업무 도구만 제공한다.

```bash
make build
./bin/archive-archivist auth login --profile-ref keychain://archive-archivist/chatgpt-default
./bin/archive-archivist auth status --profile-ref keychain://archive-archivist/chatgpt-default
./bin/archive-archivist models --profile-ref keychain://archive-archivist/chatgpt-default
```

연결이 성공하면 `models`가 반환한 model slug 하나를 [`deploy/macos/archivist-host.example.yaml`](deploy/macos/archivist-host.example.yaml)의 `--model` 값에 넣는다. Host service credential과 ChatGPT Profile은 서로 다른 Keychain reference를 사용한다. 현재 선택한 계정이나 workspace가 `chatgpt.tokens.use.direct` 권한을 제공하지 않으면 실제 추론을 시작하지 않고, 다른 지원 billing 경로를 구성하기 전까지 실계정 acceptance는 planned로 남긴다.

자체 LLM은 같은 Host에서 OpenAI 호환 `/v1/chat/completions`와 function tool call을 제공한 뒤 별도 Profile 없이 실행한다. 인증 없는 endpoint는 loopback만 허용한다. 실제 Turn은 Archivist Host가 runtime identity와 MCP URL을 넣어 실행하며, 아래 명령은 고정 인수 형태를 보여 주는 예시다.

```bash
./bin/archive-archivist run \
  --provider local \
  --api-base http://127.0.0.1:11434/v1 \
  --model REPLACE_WITH_LOCAL_TOOL_MODEL_SLUG
```

ChatGPT와 자체 LLM을 함께 둘 때는 [`deploy/macos/archivist-host.example.yaml`](deploy/macos/archivist-host.example.yaml)처럼 같은 executable을 `archive-gpt`, `archive-local` 두 CLI ID로 등록한다. 실제 자체 LLM model acceptance는 `LRH-017`로 별도 확인한다.

`reqcheck`는 다음 조건 중 하나라도 어기면 실패한다.

- 요구사항 ID 중복
- Given/When/Then, 상태 또는 테스트 수준 누락
- `implemented` 요구사항에 테스트가 없거나 둘 이상 연결됨
- `planned` 요구사항이 존재하지 않는 테스트를 구현됐다고 주장함
- 테스트의 `// Requirement: XXX-000` 주석과 카탈로그가 다름
- 같은 테스트를 둘 이상의 요구사항이 공유함

## 구현 규칙

새 동작은 다음 순서로 추가한다.

1. 설계 문서에서 책임과 경계를 결정한다.
2. `requirements/catalog.json`에 원자적인 Given/When/Then과 테스트 이름을 추가한다.
3. 같은 ID의 테스트를 하나 작성하여 실패를 확인한다.
4. Domain/Application Core에 동작을 구현한다.
5. Adapter에서 Core를 호출한다.
6. `reqcheck`와 전체 테스트를 통과시킨다.

하나의 테스트에서 여러 입력 조합을 table-driven 방식으로 확인할 수는 있지만 서로 다른 업무 결과를 한 요구사항에 묶지 않는다.
