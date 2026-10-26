# 04. 데이터와 아카이브 설계

## 1. 저장 책임

| 저장 매체 | 저장 내용 | 저장하지 않는 내용 |
|---|---|---|
| SQLite | 상태, 관계, 검색, 승인, 감사, lease | 영상 BLOB, Provider credential |
| 내부 SSD | DB, migration, 회전 로그 | 장기 영상 원본 |
| Scratch | 처리 중 임시 파일 | 확정된 유일 복사본 |
| Archive storage | 원본·파생본·sidecar·manifest | 활성 DB |
| OS Secret Store | API key/OAuth token | 영상 메타데이터 |

## 2. 논리 데이터 모델

```text
Job 1 ── N Step 1 ── N Event
 │          └────── Approval / ExecutionLease / AgentToolCall
 └── N OriginalAsset 1 ── N Artifact
                         ├── Rendition
                         ├── Transcript
                         ├── Thumbnail
                         └── QCReport

ArchivePackage 1 ── N ArchiveCopy ── Storage
PhysicalDevice 1 ── N Partition 1 ── N Volume
Volume 1 ── N Storage
Job N ── N WorkspaceDrive(role of PhysicalDevice)
```

Agent 계층은 `AgentSession → AgentTurn → ProviderRequest → AgentToolCall`로 연결하며 Tool Call은 같은 `command_id`를 사용한다.

다중 PC 실행 상태는 `WorkerDefinition → WorkerConnection/Heartbeat → ExecutionLease`로 연결한다. Worker는 DB를 직접 갱신하지 않고 Control API가 heartbeat와 결과를 transaction으로 기록한다.

## 3. 식별자와 경로

- API와 MCP 입력은 `asset_id`, `artifact_id`, `storage_id`, `volume_id`를 받는다.
- 실제 파일 접근은 Storage Adapter가 등록 root에서 연 `os.Root` capability로 수행한다. 검증된 문자열 경로를 반환한 뒤 다시 여는 방식을 사용하지 않는다.
- `..`, UNC 우회, symlink와 등록 root 밖의 경로를 거부한다. Windows junction/reparse point는 실제 Windows acceptance test인 `ARC-008`로 검증한다.
- mount path는 장치 식별자가 아니다. 장치 serial, filesystem UUID, model, capacity, 물리 라벨을 조합한다.
- path는 저장 시 정규화하되 사용자 표시용 원본 파일명도 별도 보존한다.
- 대용량 파일 Job의 작업 공간 단위는 PhysicalDevice다. Storage Adapter는 storage_id를 해당 PhysicalDevice ID로 해석하며 같은 장치의 여러 Volume과 root를 별도 작업 공간으로 취급하지 않는다. `DEV-006`

## 4. 원본과 파생본

원본은 최초 검증 체크섬을 가진 immutable `OriginalAsset`이다. 체크섬이나 위치를 바꾸지 않고 새 OriginalAsset과 대체 관계를 만든다. `ARC-001`

파생 `Artifact`는 다음 provenance가 완전해야 한다. `ARC-002`

```text
original_asset_id
input_artifact_ids
processing_kind + processor_version
profile_version
created_at + created_by
size + sha256
```

내용 검수는 provenance를 덮어쓰지 않고 대상 아티팩트 리비전을 참조하는 추가 전용 ContentReview로 기록한다. 현재 검수 상태는 가장 큰 Event 저장 순번의 기록에서 투영한다. `REV-001`

## 5. SQLite 운영 원칙

- Control Service만 write connection을 소유한다.
- WAL mode를 사용하고 DB를 네트워크 공유 경로에 두지 않는다.
- 현재 상태, CommandRecord, Event, idempotency receipt와 outbox 레코드는 한 transaction에 기록한다. `PER-001`
- 렌더링 동안 transaction을 열어두지 않는다.
- Worker progress는 제한된 주기로 집계하여 무제한 row 증가를 막는다.
- migration은 단조 증가 버전과 checksum을 가지며 시작 시 적용 상태를 검사한다.
- backup은 SQLite Online Backup 방식으로 만들고 복원 시험을 운영 절차에 포함한다.

SQLite Command Store는 `PER-001` integration test로 transaction rollback을 검증한다. 전체 projection, migration, backup/restore는 다음 수직 슬라이스에서 별도 요구사항으로 내린다.

## 6. 아카이브 패키지

```text
VP-YYYYMMDD-NNNN/
├── original/
├── archive/
├── proxy/
├── transcript/
├── thumbnail/
├── metadata.json
├── files.csv
├── summary.md
├── events.jsonl
├── manifest-sha256.txt
└── README.md
```

- `metadata.json`: 기계가 읽는 엔티티·관계·provenance
- `files.csv`: 스프레드시트로 열 수 있는 파일 목록
- `events.jsonl`: append-only 처리·보존 이벤트
- `manifest-sha256.txt`: 정렬된 상대경로와 SHA-256
- `README.md`: 도구가 없어도 읽을 수 있는 설명

Core 메타데이터 round-trip 계약은 `ARC-005`, 실제 SQLite와 디렉터리를 사용하는 sidecar package import 계약은 `ARC-007`, manifest 계약은 `ARC-003`이다.

## 7. 아카이브 확정

확정은 다음을 하나의 증적 묶음으로 검증한다. `ARC-004`

1. 현재 ArchivePackage 리비전에 대한 QCVerification 기록
2. 같은 리비전과 manifest digest에 대한 사람 Approval 기록
3. 필수 sidecar 목록과 실제 존재 목록을 가진 SidecarVerification 기록
4. manifest digest 재검증 기록
5. 서로 다른 물리 장치에 있는 두 ArchiveCopyVerification 기록
6. 모든 기록의 ID, 대상 리비전, 검증 시각과 manifest digest 일치

확정 후 수정은 기존 ArchivePackage를 덮어쓰지 않고 새 패키지가 이전 패키지를 대체하는 관계를 만든다.

## 8. DB 재구축

중앙 DB를 잃은 경우 volume catalog → package metadata → events → manifest 순으로 읽는다. import는 기존 ID와 checksum이 같은 항목은 멱등하게 병합하고 충돌은 자동 덮어쓰기하지 않으며 과거 Event를 중복 생성하지 않는다. `ARC-007`
