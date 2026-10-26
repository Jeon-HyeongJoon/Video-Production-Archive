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
 └── N OriginalAsset 1 ── N MediaInspection
                     └── N Artifact
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

원본은 `InputFileObservation`에서 최초 검증한 checksum·원래 이름·크기와 관찰 기록 ID를 고정한 immutable `OriginalAsset`이다. Storage와 상대경로는 관찰 기록 또는 사본 기록이 소유하며 OriginalAsset에 복제하지 않는다. 체크섬이나 원래 이름을 고쳐야 하면 새 OriginalAsset과 대체 관계를 만든다. `INT-002`, `ARC-001`

`MediaInspection`은 `OriginalAsset` ID뿐 아니라 확정 checksum에도 묶인 추가 전용 Record다. 파일 경로나 ffprobe 원문을 검색 기준으로 삼지 않고 정규화한 컨테이너, 길이, 크기, bitrate와 stream 기술정보를 저장한다. `MED-001`

파생 `Artifact`는 다음 provenance가 완전해야 한다. `ARC-002`

```text
original_asset_id
input_artifact_ids
processing_kind + processor_version
profile_version
created_at + created_by
size + sha256
```

`AudioArtifact`는 원본 자산에서 STT 입력 규격으로 추출한 파생 파일이다. v1은 WAV/PCM 16-bit little-endian, 16kHz, mono 규격을 사용하며 checksum·Profile·FFmpeg·Worker provenance를 가진다. `AUD-001~002`

`Transcript`는 AudioArtifact에서 STT가 게시한 JSON Artifact이면서 언어와 순서 있는 millisecond 시간 구간을 제공하는 구조화 결과다. 원본 `whisper.cpp` JSON의 host 정보와 모델 파일 경로는 게시하지 않고 Profile reference와 정규 구간만 sidecar에 남긴다. 공통 checksum·게시·provenance는 Artifact가 소유하고 전사문 검색과 타임코드 이동에 필요한 필드는 Transcript가 소유한다. `TRN-001~002`

`Subtitle`은 Transcript의 시간 구간을 versioned SRT Profile로 직렬화한 게시 Artifact다. Subtitle provenance는 입력 Transcript ID를 하나만 가지며 등록 Storage root 안의 임시 파일을 검증한 뒤 게시한다. `SUB-001~002`

`Summary`는 입력 Transcript 하나를 가리키는 게시 JSON Artifact이면서 검수 대상인 ID·리비전, 작성 주체, 요약 text·키워드·시간 챕터를 소유한다. Agent 생성과 사람 폴백은 같은 구조를 사용하고 작성 주체로 구분하며 Transcript 원문을 복제하거나 대체하지 않는다. SQLite는 저장된 Transcript 참조와 Summary ID·리비전 유일성을 확인해 추가 전용으로 보존한다. `SUM-001~003`

내용 검수는 provenance를 덮어쓰지 않고 존재하는 Summary 리비전을 참조하는 추가 전용 ContentReview로 기록한다. SQLite에서 Event 저장 순번을 고유하게 보존하고 가장 큰 순번의 기록을 현재 검수 상태로 투영한다. `REV-001~002`

## 5. SQLite 운영 원칙

- Control Service만 write connection을 소유한다.
- WAL mode를 사용하고 DB를 네트워크 공유 경로에 두지 않는다.
- 현재 상태, CommandRecord, Event, idempotency receipt와 outbox 레코드는 한 transaction에 기록한다. `PER-001`
- 렌더링 동안 transaction을 열어두지 않는다.
- Worker progress는 제한된 주기로 집계하여 무제한 row 증가를 막는다.
- migration은 단조 증가 버전과 checksum을 가지며 시작 시 적용 상태를 검사한다.
- backup은 SQLite Online Backup 방식으로 만들고 복원 시험을 운영 절차에 포함한다.

SQLite Command Store는 `PER-001` integration test로 transaction rollback을 검증한다. 전체 projection, migration, backup/restore는 다음 수직 슬라이스에서 별도 요구사항으로 내린다.

입고 카탈로그의 원본 기록은 JSON payload와 조회용 키를 함께 저장한다. `InputFileObservation`과 `OriginalAsset`은 ID 및 checksum으로, `MediaInspection`은 원본 자산 ID와 관찰 시각으로 조회한다. 인덱스 필드는 payload를 대체하는 별도 진실이 아니며 저장 시 도메인 Record와 함께 검증한다. `INT-003~004`, `MED-004`

검색 투영은 OriginalAsset별 가장 큰 CatalogRecord 리비전과 가장 최근 MediaInspection을 결합한다. 제목·설명·키워드 text, video codec과 길이 범위를 검색할 수 있지만 Storage root와 파일 상대경로는 검색 결과 계약에 포함하지 않는다. `CAT-001~002`

Transcript payload도 OriginalAsset과 게시 시각을 조회 키로 함께 저장하되 JSON payload가 진실이다. 같은 ID와 같은 내용만 멱등하게 허용하며, 전사문 검색은 OriginalAsset별 가장 최근 Transcript의 segment text에서 일치한 millisecond 구간을 반환한다. `TRN-004`, `CAT-003`

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
