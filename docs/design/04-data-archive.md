# 04. 데이터와 아카이브 설계

## 1. 저장 책임

| 저장 매체 | 저장 내용 | 저장하지 않는 내용 |
|---|---|---|
| SQLite | 상태, 관계, 검색, 승인, 감사, lease | 영상 BLOB, Provider credential |
| 내부 SSD | DB, migration, 회전 로그 | 장기 영상 원본 |
| Scratch | 처리 중 임시 파일 | 확정된 유일 복사본 |
| 아카이브 저장소 (`ARCHIVE_PRIMARY`) | 검증된 최적화 기록물·sidecar·manifest | 활성 DB, 기술적 복제본이라는 의미 |
| Archive mirror/offsite | 확정 조건을 위한 검증 복제본 | 수정 가능한 작업 결과 |
| OS Secret Store | API key/OAuth token | 영상 메타데이터 |

## 2. 논리 데이터 모델

```text
ArchiveRun 1 ── N OrganizationDecision 1 ── 0..1 Folder Job
                                            └── 1..N Step ── Event / ExecutionLease
                                                     └── organize 출력 ── ArchivePackage

Job 1 ── N Step 1 ── N Event
 │          └────── Approval / ExecutionLease / ArchivistToolCall
 └── N OriginalAsset 1 ── N MediaInspection
                     └── N Artifact
                         ├── Rendition
                         ├── Transcript
                         ├── Thumbnail
                         └── QCReport

ArchivePackage 1 ── N ArchiveCopy ── Storage
ArchivePackage 1 ── N PackageRevision(STORED → COMMITTED)
PhysicalDevice 1 ── N Partition 1 ── N Volume
Volume 1 ── N Storage
Job N ── N WorkspaceDrive(role of Volume) ── PhysicalDevice I/O guard
ArchiveRun ── Worker / Source Storage / Archive Storage / fixed FolderScope
ArchiveRun 1 ── 1 OrganizationStageOwnership / 1 TranscriptionStageOwnership
```

ArchiveRun는 기존 run 명령 영수증과 다른 영속 aggregate다. 생성할 때 담당자가 고른 참조와 고정 FolderScope를 저장하며, 단계의 시작 조건·Job 구성·완료 의미는 ArchiveRun 생성 계약에 섞지 않고 각 단계 요구사항에서 정한다. `ORG-012`

영속 객체와 조회 projection은 다음처럼 구분한다. 같은 ID를 화면마다 새 객체로 복제하지 않는다. 특히 `job_id`는 Job의 identity이므로 작업 관리·Worker·드라이브 완료 내역은 모두 같은 작업 상세 route를 참조한다.

| 영속 객체 | identity와 원장 | 주요 참조 | 조회·복구 계약 | 검증 |
|---|---|---|---|---|
| Job | `job_id`, Command Store `aggregate_states`의 revisioned `job.State` | Step, 실행 명세의 ArchiveRun·OrganizationDecision, 출력 ArchivePackage | `archive_workspace_job_projections`는 폐기 가능한 읽기 모델이며 Control 시작 시 원장에서 재구성 | `PER-001`, `DSH-001`, `CTL-003~004`, `CTL-007` |
| ArchiveRun | `archive_run_id`, Command Store의 `archiverun.State`와 멱등 Organization receipt | Worker, source/archive Storage, FolderScope, Folder Job | 드라이브 projection은 최신 실행과 전체 완료 결과를 서로 다른 범위로 계산 | `ORG-012~014`, `APP-002`, `DSH-008`, `DSH-024` |
| ArchivePackage | `package_id`, ARCHIVE_PRIMARY의 metadata·events·manifest와 import sidecar | 원본 자산, Artifact, 생산 Job Event | 중앙 DB 없이 package 자체로 복원하고 같은 ID·digest만 멱등 병합 | `ARC-003`, `ARC-007`, `ARC-011`, `ARC-013` |
| Storage·Volume·PhysicalDevice | 각 stable ID, 정규화한 topology table | Storage → Volume → Partition → PhysicalDevice | 경로는 공개 identity가 아니며 StorageRoot capability로만 접근 | `DEV-006~008`, `CFG-006` |
| OriginalAsset | `asset_id`, checksum과 최초 observation을 가진 catalog table | InputFileObservation, MediaInspection, Artifact | 추가 전용 기록과 최신 검색 projection을 분리 | `INT-002~004`, `MED-004`, `CAT-002` |
| OrganizationDecision | `decision_id`와 sequence, `archive_run_id` FK 및 immutable payload | MediaFolder input revision, 원본·검사, 선택 Profile | 최신 판정은 sequence로 선택하고 과거 판정을 수정하지 않음 | `ORG-002~003` |
| ArchivistSession·Turn | `session_id`·`turn_id`, 정규화한 Archivist table과 FK | Host, ToolCall, ActionCard, ArchiveRun 업무 문맥 | 비밀과 raw terminal을 제외한 대화 projection | `LRH-001`, `LRH-012~013`, `DSH-002`, `DSH-011` |

Command Store는 서로 다른 aggregate를 같은 `aggregate_states` 표에 JSON 상태로 보관하므로 ArchiveRun과 Job 사이를 SQL foreign key로 묶지 않는다. 이 경계의 참조 무결성은 생성 Command, 결정적 stable ID, 저장된 실행 명세와 출력 기록으로 검증한다. 반면 한 aggregate 안의 구성과 정규화 저장소 내부 관계는 FK 또는 Core validation으로 강제한다. 읽기 projection 손실을 원장 손실로 취급하지 않으며 Job projection은 시작 시 복구한다. `CTL-007`

Archivist 계층은 `ArchivistSession → ArchivistTurn → ProviderRequest → ArchivistToolCall`로 연결하며 Tool Call은 같은 `command_id`를 사용한다.

다중 PC 실행 상태는 `WorkerDefinition → WorkerConnection/Heartbeat → ExecutionLease`로 연결한다. Worker는 DB를 직접 갱신하지 않고 Control API가 heartbeat와 결과를 transaction으로 기록한다.

## 3. 식별자와 경로

- API와 MCP 입력은 `asset_id`, `artifact_id`, `storage_id`, `volume_id`를 받는다.
- 실제 파일 접근은 Storage Adapter가 등록 root에서 연 `os.Root` capability로 수행한다. 검증된 문자열 경로를 반환한 뒤 다시 여는 방식을 사용하지 않는다.
- `..`, UNC 우회, symlink와 등록 root 밖의 경로를 거부한다. Windows junction/reparse point는 실제 Windows acceptance test인 `ARC-008`로 검증한다.
- mount path는 장치 식별자가 아니다. 장치 serial, filesystem UUID, model, capacity, 물리 라벨을 조합한다.
- path는 저장 시 정규화하되 사용자 표시용 원본 파일명도 별도 보존한다.
- 대용량 파일 Job의 작업 공간 단위는 논리 Volume이다. Storage Adapter는 storage_id를 stable volume ID의 workspace_drive_id로 해석하며 같은 Volume의 여러 root는 같은 작업 공간, 같은 PhysicalDevice의 서로 다른 Volume은 다른 작업 공간으로 취급한다. PhysicalDevice ID는 동시 I/O 충돌과 안전 제거 판정용 보조 키로 별도 반환한다. `DEV-006`

Storage는 로컬 root, 연결된 네트워크 공유 root, 새 외장 data volume root 또는 FTP·SFTP 원격 파일 서비스 root일 수 있다. 이 차이는 `StorageAccessKind.LOCAL`, `MOUNTED_NETWORK`, `ATTACHED_VOLUME`, `FTP`, `SFTP`로 표시하며 StorageRole과 섞지 않는다. API·MCP·업무 상태는 모든 경우 `storage_id`만 사용하고 절대경로·endpoint·credential 원문을 노출하지 않는다. 새 외장 Volume은 Worker가 자동 관찰하고 같은 root에도 SOURCE_MEDIA와 ARCHIVE_PRIMARY의 서로 다른 stable Storage ID를 만든다. 원본과 다른 논리 Volume의 ARCHIVE_PRIMARY ID만 아카이브 후보가 되며 담당자가 직접 고른다. 같은 PhysicalDevice의 별도 Volume도 후보가 될 수 있지만 Scheduler는 물리 I/O 충돌 키로 동시 heavy 작업을 막는다. 네트워크 공유의 identity·UNC·credential 방식은 `CFG-012`, FTP·SFTP의 endpoint identity·Secret Store reference·안전 게시 기능은 `CFG-013` acceptance 전까지 `planned`다.

원격 파일 서비스 Adapter는 임시 업로드, 게시 전후 다시 읽기, 목록, manifest checksum 검증과 원자 rename 또는 그와 동등한 안전 게시를 제공해야 한다. 해당 기능을 확인하지 못한 후보는 화면에 접근 불가로 투영하고 아카이브 실행에 선택할 수 없다. SMTP는 파일 namespace의 목록·재검증·패키지 리비전 게시를 제공하지 않으므로 아카이브 저장소가 아니다. 완료 보고서나 아카이브 참조를 보내는 전달 채널로만 별도 설계한다.

로컬·mount 기반 실제 게시의 공통 경계는 `archive.StorageRoot`다. 애플리케이션과 개별 Adapter는 검증한 문자열 경로를 unrestricted `os` 호출로 다시 열지 않는다. 임시 출력은 같은 root에서 파일 동기화와 checksum·형식 검증을 마친 뒤 `os.Root.Rename`으로 게시하고 상위 디렉터리를 동기화한다. Windows 파일 동기화는 `FlushFileBuffers`가 허용되는 쓰기 가능한 root 제한 핸들을 사용한다. subprocess가 만든 임시 출력도 같은 게시 경계를 통과하며 실패한 `.partial`은 다음 재시도 전에 제거한다. `ARC-005`, `ARC-014`, `ORG-008~009`, `WRK-012`

## 4. 원본과 파생본

원본은 `InputFileObservation`에서 최초 검증한 checksum·원래 이름·크기와 관찰 기록 ID를 고정한 immutable `OriginalAsset`이다. Storage와 상대경로는 관찰 기록 또는 사본 기록이 소유하며 OriginalAsset에 복제하지 않는다. 체크섬이나 원래 이름을 고쳐야 하면 새 OriginalAsset과 대체 관계를 만든다. `INT-002`, `ARC-001`

`MediaInspection`은 `OriginalAsset` ID뿐 아니라 확정 checksum에도 묶인 추가 전용 Record다. 파일 경로나 ffprobe 원문을 검색 기준으로 삼지 않고 정규화한 컨테이너, 길이, 크기, 전체 bitrate와 컨테이너 생성 시각, stream별 codec·profile·frame rate·pixel format·bitrate·audio 규격을 저장한다. 컨테이너 생성 시각이 없거나 잘못됐으면 stream tag나 파일시스템 날짜로 대체하지 않는다. `MED-001`, `MED-005~007`

`OrganizationDecision`은 작업 공간 드라이브와 MediaFolder 입력 revision, policy version, 정렬된 OriginalAsset·MediaInspection 참조, OrganizationDisposition, 선택 Profile, CutMarker, STT 가능 여부와 보류 사유를 가진 추가 전용 Record다. 사람 또는 Archivist의 명시적 분류는 기존 Record를 고치지 않고 ActorType·행위자 ID·사유·근거가 있는 새 sequence를 추가한다. `ORG-002~003`

`SourceTimeline`은 여러 OriginalAsset의 Clip 순서와 in/out, CutMarker를 가진 리비전 대상 Entity다. 이를 입력으로 만든 Rendition provenance는 Timeline ID와 모든 root OriginalAsset ID를 가진다. 단일 원본 파생물은 기존 `original_asset_id` 계약을 유지하지만 다중 원본 파생물을 첫 원본 하나로 축약하지 않는다. `ORG-006`, `ARC-009`

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

`Summary`는 입력 Transcript 하나를 가리키는 게시 JSON Artifact이면서 검수 대상인 ID·리비전, 작성 주체, 요약 text·키워드·시간 챕터를 소유한다. Archivist 생성과 사람 폴백은 같은 구조를 사용하고 작성 주체로 구분하며 Transcript 원문을 복제하거나 대체하지 않는다. SQLite는 저장된 Transcript 참조와 Summary ID·리비전 유일성을 확인해 추가 전용으로 보존한다. `SUM-001~003`

내용 검수는 provenance를 덮어쓰지 않고 존재하는 Summary 리비전을 참조하는 추가 전용 ContentReview로 기록한다. SQLite에서 Event 저장 순번을 고유하게 보존하고 가장 큰 순번의 기록을 현재 검수 상태로 투영한다. `REV-001~002`

## 5. SQLite 운영 원칙

- Archive Control만 write connection을 소유한다.
- WAL mode를 사용하고 DB를 네트워크 공유 경로에 두지 않는다.
- 현재 상태, CommandRecord, Event, idempotency receipt와 outbox 레코드는 한 transaction에 기록한다. `PER-001`
- 같은 DB를 여는 runtime 저장소는 정규화한 경로별 process-wide `sql.DB` 하나와 reference-counted 수명주기를 공유한다. 개별 저장소가 독립 pool을 만들거나 다른 저장소가 쓰는 연결을 먼저 닫지 않는다. `PER-002`
- 렌더링 동안 transaction을 열어두지 않는다.
- Worker progress는 제한된 주기로 집계하여 무제한 row 증가를 막는다.
- migration은 단조 증가 버전과 checksum을 가지며 시작 시 적용 상태를 검사한다.
- backup은 SQLite Online Backup 방식으로 만들고 복원 시험을 운영 절차에 포함한다.

SQLite Command Store는 `PER-001` integration test로 transaction rollback을, `PER-002`로 공유 연결 수명주기를 검증한다. row cursor를 연 채 같은 단일 연결로 후속 조회하지 않고 먼저 필요한 행을 버퍼링해 닫는다. 조회용 시각 문자열은 고정 9자리 소수부의 UTC 형식으로 기록해 문자열 정렬과 실제 시각 순서를 일치시키며, lease 만료 판정은 저장 문자열을 `time.Time`으로 파싱해 비교한다. Job 읽기 projection은 Control 시작 때 원장의 모든 유효 Job aggregate에서 다시 만들며 다른 aggregate를 Job으로 오인하지 않는다. DB 전체 migration과 backup/restore는 다음 수직 슬라이스에서 별도 요구사항으로 내린다. `CTL-007`

Archive Control 시작 시 boot 설정의 Storage topology를 같은 SQLite DB에 등록한다. 동일한 physical device·partition·volume·Storage ID·root 재등록은 재시작으로 보고 멱등하게 허용하지만 기존 ID의 parent나 root가 달라지면 시작을 중단한다. `CFG-006`, `CTL-002`

입고 카탈로그의 원본 기록은 JSON payload와 조회용 키를 함께 저장한다. `InputFileObservation`과 `OriginalAsset`은 ID 및 checksum으로, `MediaInspection`은 원본 자산 ID와 관찰 시각으로 조회한다. 인덱스 필드는 payload를 대체하는 별도 진실이 아니며 저장 시 도메인 Record와 함께 검증한다. `INT-003~004`, `MED-004`

검색 투영은 OriginalAsset별 가장 큰 CatalogRecord 리비전과 가장 최근 MediaInspection을 결합한다. 제목·설명·키워드 text, video codec과 길이 범위를 검색할 수 있지만 Storage root와 파일 상대경로는 검색 결과 계약에 포함하지 않는다. `CAT-001~002`

Transcript payload도 OriginalAsset과 게시 시각을 조회 키로 함께 저장하되 JSON payload가 진실이다. 같은 ID와 같은 내용만 멱등하게 허용하며, 전사문 검색은 OriginalAsset별 가장 최근 Transcript의 segment text에서 일치한 millisecond 구간을 반환한다. `TRN-004`, `CAT-003`

## 6. 아카이브 패키지와 보관됨

Archive Intake의 ArchivePackage는 결과 영상 하나당 하나다. SOURCE_MEDIA 원본 파일은 복사하지 않고 원본 드라이브 identity·원래 이름·상대경로·checksum·길이를 metadata에 남긴다.

layout v1은 기존 기록의 읽기 호환과 복원을 위해 유지한다.

```text
<아카이브 저장소>/
├── packages/
│   └── YYYY/MM/DD/
│       └── VP-YYYYMMDD-NNNN/
│           ├── <원본 영상명 또는 MediaFolder 이름>.mp4
│           ├── transcript.json
│           ├── transcript.txt
│           ├── subtitle.srt
│           ├── metadata.json
│           ├── files.csv
│           ├── events.jsonl
│           ├── manifest-sha256.txt
│           └── README.md
└── archive-catalog.csv
```

패키지 ID는 멱등성과 참조 무결성을 위한 불변 기계 식별자이고 디렉터리는 ID의 처리 날짜로 `YYYY/MM/DD`까지 나눈다. 결과 영상은 단일 입력이면 원래 영상명의 확장자를 `.mp4`로 바꾸고, 여러 컷 통합이면 MediaFolder의 마지막 이름을 사용한다. Windows 금지문자·예약어·후행 점과 공백·과도한 길이는 결정적으로 안전한 이름으로 정규화한다. `metadata.json`은 처리 날짜, MediaFolder, 표시명, 실제 결과 영상 파일명과 함께 원래 이름·전체 상대경로·패키지 ID를 보존하므로 사람이 읽는 이름을 바꾸어도 identity를 잃지 않는다. `ARC-013`

새 기본 게시 형식인 layout v3의 구조는 다음과 같다. 저장소 root 바로 아래 결과 폴더 하나만 두고, 사람이 직접 쓰는 결과 영상·영상 정보·텍스트·자막만 공개한다. 복구·무결성·리비전 계약은 숨김 문서 하나로 합친다. `packages/YYYY/MM/DD`, `revisions/<revision>` 같은 공개 하위 디렉터리는 만들지 않는다. 기존 layout v1·v2 패키지는 자동 이동하거나 이름을 바꾸지 않고 reader를 유지한다. `ARC-015`

```text
<아카이브 저장소>/
├── archive-catalog.csv
└── 2026-11-09_행사명__a1b2/
    ├── 행사명.mp4
    ├── video-info.csv
    ├── transcript.txt          # 2단계 뒤 추가
    ├── subtitle.srt            # 2단계 뒤 추가
    └── .archive-package.json
```

layout v3의 결과 폴더 이름은 `처리일_사람이 읽는 결과명__패키지 ID suffix`로 결정적으로 만들고, `.archive-package.json`의 package ID가 identity다. 폴더 이름은 identity가 아니다. 공개 파일은 현재 리비전 projection이다. `.archive-package.json`은 package metadata, 원본 provenance, 현재 상태, checksum manifest, event 의미, 구조화 Transcript와 과거 리비전의 텍스트 bytes를 `revisions[]`에 보존한다. 새 공개 파일을 임시 파일로 검증·교체한 뒤 이 문서를 마지막에 원자 교체하므로 별도 README·files.csv·events.jsonl·manifest·current·`.r*` 파일은 만들지 않는다. `ARC-016`

- `video-info.csv`: 이 결과를 만든 OriginalAsset별 원래 이름·상대경로·checksum·길이·크기·컨테이너·video/audio stream 기술정보와 판정
- `transcript.txt`, `subtitle.srt`: STT 뒤 사람이 직접 읽고 사용하는 현재 결과
- `.archive-package.json`: 기계가 읽는 identity·provenance·checksum·리비전·복구 기록. 구조화 Transcript도 여기에 포함한다.

Archive Intake의 1단계 결과는 담당자가 고른 `ARCHIVE_PRIMARY`에 ArchivePackage로 게시한다. 한 벌의 checksum 기록까지 검증되면 `STORED`, 즉 보관됨이다. 2단계 Transcript·TranscriptText·Subtitle은 확정 전 패키지에 checksum manifest가 다른 새 리비전으로 추가한다. ArchiveCatalog는 같은 `ARCHIVE_PRIMARY`를 쓰는 작업과 원본 드라이브를 가리지 않고 최상단 `archive-catalog.csv` 하나에 누적한다. Worker와 process가 달라도 같은 StorageRoot의 잠금 파일을 배타적으로 잡은 뒤 현재 투영을 읽고 병합해 `archive-catalog.csv.partial`에 작성·동기화·검증한 뒤 `os.Root.Rename`으로 원자 교체한다. 기존 `catalog/<원본 드라이브 ID>.csv`가 있고 최상단 파일이 없으면 첫 게시에서 행을 병합하고 기존 파일은 복구 근거로 유지한다. `ORG-008~009`, `ARC-011`

layout v3에서는 검색과 상세 provenance의 범위를 분리한다. 루트 `archive-catalog.csv`는 ArchivePackage, 즉 결과 폴더 하나당 한 행인 저장소 전체 검색 projection이다. 패키지 폴더의 `video-info.csv`는 그 결과를 만든 OriginalAsset 하나당 한 행인 상세 기록이며 다중 소스 결과는 여러 행을 가진다. `.archive-package.json`은 검색 인덱스가 아니라 package identity·layout version·현재 revision·원본 참조·provenance·복구에 필요한 기계 계약이다. 따라서 실제 매핑은 `결과 폴더 1 = ArchivePackage 1 = 결과 영상 1 = 루트 CSV 행 1`이고, 그 안의 `video-info.csv`만 `OriginalAsset N`을 표현한다. `MED-008`, `ORG-015`

`archive-media catalog verify`는 root의 1단계 결과 폴더만 열어 layout version에 맞는 복구 문서와 checksum을 검증하고 package ID, current revision, 결과 영상 정확히 하나, `video-info.csv` 원본 수와 루트 CSV 행을 양방향으로 대조한다. 누락·고아·중복·값 불일치·잘못된 깊이를 각각 `CATALOG_MISSING`, `CATALOG_ORPHAN`, `CATALOG_MISMATCH`, `LAYOUT_INVALID`로 보고하고 어떤 파일도 바꾸지 않는다. `archive-media catalog rebuild`는 유효한 versioned 복구 문서에서 루트 CSV 전체를 임시 파일로 다시 만든 뒤 schema·행 유일성·모든 폴더의 1:1 대응을 검증하고 원자 교체한다. 패키지 폴더를 수정·삭제하지 않는다. `ARC-017~018`

Core 메타데이터 round-trip 계약은 `ARC-005`, manifest 계약은 `ARC-003`이다. SQLite와 디렉터리를 사용하는 sidecar package import prototype은 있으나 Control의 복원 명령에 연결되지 않았으므로 `ARC-007`은 planned다.

## 7. 아카이브 확정

보관됨과 확정됨은 같은 상태가 아니다. 보관됨은 ARCHIVE_PRIMARY 한 벌의 검증 게시이고, 확정됨은 아래 `ARC-004` 증적을 모두 만족한 `COMMITTED` 상태다. 화면 이름 “아카이브”와 05 검색은 두 상태의 같은 기록물을 보되 상태를 구분한다.

확정은 다음을 하나의 증적 묶음으로 검증한다. `ARC-004`

1. 현재 ArchivePackage 리비전에 대한 QCVerification 기록
2. 같은 리비전과 manifest digest에 대한 사람 Approval 기록
3. 필수 sidecar 목록과 실제 존재 목록을 가진 SidecarVerification 기록
4. manifest digest 재검증 기록
5. 서로 다른 물리 장치에 있는 두 ArchiveCopyVerification 기록
6. 모든 기록의 ID, 대상 리비전, 검증 시각과 manifest digest 일치

확정 후 수정은 기존 ArchivePackage를 덮어쓰지 않고 새 패키지가 이전 패키지를 대체하는 관계를 만든다.

확정은 브라우저가 증적 payload를 올리거나 상태만 바꾸는 동기 API가 아니다. 담당자가 다른 물리 장치의 `ARCHIVE_MIRROR` 또는 `BACKUP_OFFSITE`를 고르면 Worker가 현재 리비전을 복제하고 양쪽 manifest·sidecar·QC를 검증한다. 그 manifest digest에 묶인 사람 Approval을 별도 ActionCard 조작으로 남긴 뒤 `CommitArchive`가 양쪽 current projection을 신규 임시 파일에 쓰고 검증한 후 교체한다. 복제·검증·승인·확정은 각각 멱등키 기록을 남기며, 어느 단계든 실패하면 `STORED`를 유지한다. 이 수직 흐름은 실제 서로 다른 두 물리 장치 acceptance 증적을 남기기 전까지 `ARC-012` planned로 둔다.

## 8. DB 재구축

중앙 DB를 잃은 경우 ArchiveCatalog를 빠른 목록으로 읽되 진실로 간주하지 않는다. layout version별 복구 문서와 checksum을 검증하고 `video-info.csv`의 OriginalAsset 참조를 복원한 뒤 ArchiveCatalog를 대조하거나 다시 만든다. layout v2·v3 대조와 재생성은 typed MediaOperation과 CLI에 연결되어 있다. import는 기존 ID와 checksum이 같은 항목은 멱등하게 병합하고 충돌은 자동 덮어쓰기하지 않으며 과거 Event를 중복 생성하지 않는다. 이 import 흐름은 제품 명령과 실제 패키지에 연결하는 integration test가 생길 때까지 `ARC-007` planned다. `ARC-017~018`
