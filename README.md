# Video Production Archive

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
- 사람·에이전트 권한 정책
- 내부 CIDR allowlist, 선택형 로그인, 무로그인 권한 축소와 CSRF/Origin 방어
- 시작 설정 전용 관리 모델과 여러 작업 PC Worker registry·heartbeat·배정 정책
- 요구사항과 테스트의 자동 1:1 추적 검사
- 등록 Storage 입고·기술정보 분석·검증된 렌더·통합 QC·메타데이터 검색과 timestamp 전사 계약

현재는 SQLite Command Unit of Work, Storage→PhysicalDevice 작업 공간 해석, 아카이브 sidecar export/import, 게시 장애 조정, Notification SSE 재연결, 독립 Capability 실행·증적·Dashboard projection과 CI 테스트 증적의 integration 계약까지 구현되어 있다. 등록 Storage 입고 카탈로그와 ffprobe·FFmpeg·whisper.cpp subprocess Adapter도 격리 fixture로 검증하며 FFmpeg speech audio 추출에서 timestamp Transcript 게시까지 연결된다. 전체 Control DB projection·migration·backup, HTTP Command API와 웹 Dashboard, MCP Adapter, Windows Device Adapter, VJ·요약 기능과 Provider API Adapter는 아직 구현하지 않았다. 실제 Windows 장치와 운영 ffprobe·FFmpeg·whisper.cpp 실행은 장비가 제공될 때까지 acceptance 계획 상태다.

## 문서 읽는 순서

1. [설계 인덱스](docs/design/00-overview.md)
2. [시스템 컨텍스트](docs/design/01-system-context.md)
3. [컨테이너와 경계](docs/design/02-containers.md)
4. [도메인과 워크플로](docs/design/03-domain-workflow.md)
5. [데이터와 아카이브](docs/design/04-data-archive.md)
6. [실행·자원·배포](docs/design/05-runtime-deployment.md)
7. [에이전트와 보안](docs/design/06-agent-security.md)
8. [다중 Worker 운영](docs/design/07-worker-fleet.md)
9. [이벤트 스토밍](docs/design/08-event-storming.md)
10. [Dashboard 운영 투영](docs/design/09-dashboard-operations.md)
11. [도메인 기능 실행과 검증 증적](docs/design/10-capabilities-observability.md)
12. [요구사항 운영 규칙](requirements/README.md)
13. [기계 판독 요구사항 카탈로그](requirements/catalog.json)

초기 설정 예시는 [config.example.yaml](config.example.yaml)에 있다. 실제 credential은 이 파일에 쓰지 않고 Windows Credential Manager 또는 DPAPI reference만 기록한다.

## 검증

```bash
go run ./cmd/reqcheck
go run ./cmd/ontocheck
go test ./...
```

`ontocheck`는 [`ontology/terms.json`](ontology/terms.json)의 용어 정의가 서로 모순되지 않는지, 금지된 코드 식별자가 쓰이지 않는지 검사한다. 한 단어가 두 용어를 가리키거나, 아직 결정되지 않은 다의어(`open`)가 결정된 용어와 겹치면 실패한다. 용어 결정 때문에 코드·요구사항·문서에서 바꿔야 할 것은 `follow_ups`에 기록하고, 코드 이름을 바꾼 뒤에는 옛 이름을 `forbidden.code`로 옮긴다.

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
