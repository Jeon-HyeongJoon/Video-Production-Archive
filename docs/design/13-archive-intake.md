# 13. Archive Intake 설계

## 1. 목적과 결정 경계

담당자는 Worker PC에 연결된 원본 드라이브에서 폴더 범위를 고르고, 같은 Worker가 접근하는 아카이브 저장소에 최적화 기록물을 보관한다. 이 업무는 스냅샷 복제가 아니다. 원본 추적 정보와 검증된 결과를 사람이 관리·검색·복원할 수 있는 ArchivePackage와 ArchiveCatalog로 남기는 일이다.

2026-10-31에 다음을 결정했다.

- 아카이브 저장소는 `StorageRole.ARCHIVE_PRIMARY`다. 현장 표현인 “백업 하드”도 이 뜻이다. 기술적 복제본은 `BACKUP_OFFSITE`에만 둔다.
- 화면 이름은 “아카이브”이며 검색 화면과 같은 기록물을 가리킨다.
- 로컬 Storage, 연결된 네트워크 공유, 원본과 다른 새 외장 드라이브 및 검증된 FTP·SFTP 원격 파일 서비스 Storage를 허용한다. SMTP는 아카이브 저장소가 아니라 전달 채널이다.
- 담당자만 Worker·원본 드라이브·아카이브 저장소·폴더 범위·STT 시작 시점을 고른다. Archivist와 자동 정책은 Storage나 처리 범위를 고르지 않는다. 담당자가 고정한 범위 안의 HOLD는 허용된 제어 모드에서 Archivist가 구조화된 근거로 명시적으로 분류할 수 있다.
- ARCHIVE_PRIMARY에 한 벌을 검증해 게시하면 “보관됨”이다. `ARC-004`의 복제본 검증과 사람 승인을 충족하면 “확정됨”이다.
- 기록물은 중앙 DB 없이 복원 가능한 `ARC-007` ArchivePackage 형식을 따른다.
- 원본 드라이브 분리와 네트워크 아카이브 저장소 끊김은 `WaitReason.DEVICE`다. 기다리는 동안 실행 시도 횟수를 소비하지 않는다.
- 폴더별 단계 표시는 2026-10-31에 막대 매트릭스 대신 드라이브당 처리 흐름 그래프와 진행 피드백으로 바꿨다. 단계별 세부는 노드와 폴더 행 세부 설명에서 확인한다.

아카이브 실행 개체는 기존 run 영수증과 분리된 영속 `ArchiveRun`다. 2026-10-31에 Q1~Q12의 권장안을 모두 v1 계약으로 확정했다. `ORG-012`

## 2. 선택과 탐색

담당자가 고르는 순서는 Worker, 원본 드라이브, 아카이브 저장소, 폴더 범위다. 상단 Worker 선택은 목록 필터일 뿐 업무 Event를 만들지 않는다. `DEV-010`, `DSH-010`

Windows에서 외부 data bus Volume이 연결되면 Worker는 파일을 읽지 않고 PhysicalDevice·volume identity, 역할이 다른 source·archive Storage identity, label·filesystem·capacity만 후보로 보고한다. USB bridge가 volume을 `DRIVE_FIXED`로 노출해도 외부 bus면 후보이며 내부 시스템 volume과 작은 보조 partition은 제외한다. drive letter와 mount root는 Worker 밖으로 보내지 않는다. `DEV-007`, `DEV-010`

원본은 사람이 현재 candidate revision을 고른 뒤에만 해당 Worker에서 `SOURCE_MEDIA` ReadOnlyStorageRoot로 개방한다. Control은 root 없이 ArchiveIntakeSession의 stable identity를 기록하고 선택 storage ID만 heartbeat 응답으로 Worker에 돌려준다. 아카이브 저장소는 같은 Worker가 접근할 수 있는 `ARCHIVE_PRIMARY`이며 원본과 다른 논리 Volume이어야 한다. 새 외장 Volume은 원본용 Storage ID와 별도의 archive Storage ID로 후보에 나타나므로 역할이 섞이지 않는다. Worker projection은 로컬 root, 연결된 네트워크 공유, 원본과 다른 새 외장 Volume, 검증된 FTP·SFTP를 `StorageAccessKind`로 구분한다. 담당자가 고르기 전에는 새 외장 Volume에 쓰지 않는다. 접근 불가 원격 서비스와 예상 결과 크기보다 여유 용량이 부족한 후보는 시작을 막는다. 유효한 시작은 아카이브 Storage 단위 쓰기 예약을 획득한다. Archivist나 정책은 두 Storage 중 하나를 대신 고르지 않는다. `DEV-008`, `DEV-010`, `ARC-010`, `APP-006`, `CFG-012~013`

Worker는 현재 연결된 외장 Volume의 두 역할 ID를 로컬 `StorageRoot` capability로만 해석하고 root 경로를 Control에 보내지 않는다. capability를 여는 것만으로 파일을 읽거나 쓰지 않으며, 담당자가 선택한 뒤 생성된 fenced 실행 명세만 archive ID에 게시할 수 있다. 분리 시 실행 중 handle을 닫지 않고 유휴 조정에서 동적 capability를 제거한다. `DEV-013`

폴더 탐색과 FFprobe 검사는 원본에 접근하는 Worker에서 실행한다. 선택 직후 folder inventory는 background에서 실행해 대용량 탐색 중에도 heartbeat와 자원 수용 판정을 멈추지 않는다. 지원 regular file만 즉시 상위 정규화 상대 폴더로 묶고, 보호·숨김 항목, symlink, junction·reparse 후보, irregular entry와 root 밖 대상은 따라가지 않는다. 실행 전 heartbeat inventory는 폴더 ID·영상 수·입력 byte와 탐색 revision만 보내 미분류 선택 화면을 만들고, 아카이브 실행 검사는 각 영상의 정규화한 기술정보를 추가한다. Control에는 Worker의 절대경로와 executable 경로를 보내지 않는다. `DEV-010`, `ORG-001`, `CFG-011`, `ORG-010`

탐색 결과는 단조 revision을 가진다. 일부 선택과 전체 선택 모두 시작 시점 revision의 폴더 ID 집합으로 고정한다. 이후 파일 추가·삭제가 감지되면 오래된 시작·override를 거부하고 원본 드라이브 상태를 “내용 바뀜”으로 투영한다. `ORG-011`, `APP-002`

완료된 ArchiveRun은 최신 탐색 목록을 소비하지 않는다. 담당자는 같은 원본 revision의 폴더를 새 ArchiveRun ID로 다시 선택할 수 있다. 이때 checksum·상대경로가 같은 입력의 Observation과 OriginalAsset identity는 재사용하지만 새 검사는 새 MediaInspection으로 기록하고 OrganizationDecision·Folder Job·예상 출력은 새 ArchiveRun identity를 포함해 이전 실행과 분리한다. 같은 ArchiveRun ID와 멱등성 key의 재전송만 기존 receipt를 반환한다. `APP-002`

## 3. 규칙 기반 분류

자동 분류는 파일명이나 전사문 의미를 해석하지 않고 MediaInspection과 versioned policy만 사용한다. Archivist의 HOLD 검토 추론은 자동 분류와 분리된 명시적 결정으로 기록한다.

| 판정 | 조건 | 1단계 결과 |
|---|---|---|
| `ORGANIZE_AS_IS` | 1080p와 승인 bitrate·codec 범위에 이미 맞음 | 검증 뒤 그대로 보관 |
| `RENDER_RECORDINGS` | 일반 녹화영상이며 새 렌더 필요 | 기본 1080p 6Mbps, 명시 선택 시 20Mbps |
| `COMPILE_SOURCE_SEQUENCE` | 짧은 여러 Clip의 소스 폴더 | 입력 순서·CutMarker를 고정해 1080p 20Mbps 통합 |
| `HOLD` | 하나의 결정으로 수렴하지 않음 | `WAITING/INPUT`, 외부 실행 없음 |

HOLD도 OriginalAsset별 ArchiveCatalog 행과 보류 사유를 만든다. 사람 또는 Archivist의 명시적 분류는 최신 탐색 revision을 요구하며 기존 OrganizationDecision을 수정하지 않고 ActorType·행위자 ID·사유·근거가 있는 새 sequence를 추가한다. 자동 분류는 계속 결정적으로 유지하고 Archivist 추론을 입력으로 섞지 않는다. 구조화된 근거로도 판정할 수 없으면 새 결정을 만들지 않고 HOLD를 유지한다. 비HOLD 결정이 추가되면 같은 ArchiveRun의 고정 Worker·Storage·PhysicalDevice·입력 revision으로 후속 1단계 Folder Job을 멱등하게 연결한다. timeout 재전송은 결정과 Job 효과를 중복하지 않는다. 다른 폴더는 HOLD와 독립적으로 계속 실행한다. `ORG-003`, `ORG-014`

Archivist의 정리 검토 단위는 현재 HOLD인 MediaFolder 하나다. 목록에는 선택 범위 안의 HOLD만 두고, 단일 상세에는 최신 decision ID·입력 revision·보류 사유와 원본별 정규화 상대경로·checksum·duration·codec·해상도·bitrate를 제공한다. Storage root, credential, 임의 path 읽기 도구는 제공하지 않는다. 파일명과 미디어에서 파생된 내용은 분류 근거일 수 있지만 지시나 Approval로 해석하지 않는다. `MCP-004`

## 4. 1단계: 검사·분류·렌더·QC·보관

1단계 Folder Job은 STT를 포함하지 않는다. 폴더별 Step은 다음 순서다. `PIP-007`

```text
분류 → 마커 → 렌더 → QC → 보관
```

- 그대로 사용은 분류 뒤 QC·보관으로 간다.
- 개별 렌더는 6Mbps 또는 20Mbps 렌더 뒤 QC·보관으로 간다.
- 소스 시퀀스는 정렬된 Clip과 CutMarker를 고정하고 20Mbps 통합 렌더 뒤 QC·보관으로 간다.
- HOLD는 `WaitReason.INPUT`만 기록하고 외부 Step을 만들지 않는다.

Worker는 외부 subprocess에 executable과 argument 배열만 넘기고 StorageRoot 안 임시 출력에 쓴다. QC와 checksum 검증 뒤에만 선택한 ARCHIVE_PRIMARY에 게시한다. 한 벌이 검증되면 `ArchivePackageState.STORED`, 화면의 “보관됨”이다. 아직 Transcript·Subtitle을 새 패키지 리비전으로 더할 수 있다. `ORG-008`, `ARC-011`

손상 입력·지원하지 않는 codec은 해당 영상만 실패로 종결하고 ArchiveCatalog에 실패 상태를 남긴다. 일시 process 오류는 재시도 횟수를 소비한다. 장치·네트워크 Storage 불가와 실행 중 아카이브 저장소 용량 부족은 사람이 복구할 때까지 `WAITING/DEVICE`이고 횟수를 소비하지 않는다. `DEV-012`

## 5. 2단계: STT

STT 시작은 별도 Command다. 해당 ArchiveRun의 1단계에 실행 중이거나 재시도 예정인 작업이 없을 때만 받는다. HOLD는 명시적 분류 입력을 기다리므로 남은 실행 가능 작업 수와 2단계 대상에서 제외한다. 다른 ArchiveRun와 시스템 전체 queue는 이 게이트에 포함하지 않고 자원 경합은 Scheduler가 처리한다. `PIP-008`

대상은 1단계에서 보관됨이 된 `ORGANIZE_AS_IS`·`RENDER_RECORDINGS` 녹화영상뿐이다. 소스 시퀀스, HOLD, 실패 입력은 제외한다. `ORG-007`

대상 기록물마다 1단계 Folder Job과 분리된 STT Job을 만들며 폴더별 2단계 Step은 다음 순서다. `PIP-009`

```text
오디오 → STT → 자막·텍스트 → 기록물에 추가 → ArchiveCatalog
```

AudioArtifact는 16kHz mono PCM 규격과 provenance를 보존한다. Transcript는 정규화한 시간 구간 JSON, Subtitle은 SRT이며 `transcript.txt`는 같은 구간 text를 시간순으로 합친 사람이 읽는 UTF-8 sidecar다. 세 파일을 같은 ArchivePackage에 추가할 때 기존 리비전을 고치지 않고 새 manifest digest의 리비전을 게시한다. `ORG-008`, `ARC-011`

Transcript 게시가 성공하면 Worker는 versioned 결정 규칙으로 구간 길이와 문자 밀도를 평가하고 판정을 transcript Step의 출력 기록에 함께 보낸다. `REVIEW_RECOMMENDED`는 2단계 완료를 막지 않으며 폴더별 확인 필요 항목으로 남는다. 담당자는 원본·전사 텍스트·자막을 대조할 대상을 이 신호로 찾고, 화면은 저장된 사유와 측정값만 표시한다. `TRN-007`, `DSH-018`

## 6. ArchivePackage와 ArchiveCatalog

아카이브 저장소의 논리 구조는 다음과 같다. 결과 영상 하나가 ArchivePackage 하나를 소유하며 SOURCE_MEDIA 원본 파일은 넣지 않는다. 대신 원본 드라이브 ID, 원래 이름, 상대경로, checksum과 길이를 metadata에 기록한다.

```text
<아카이브 저장소>/
├── packages/
│   └── VP-20261031-0001/
│       ├── video.mp4
│       ├── transcript.json
│       ├── transcript.txt
│       ├── subtitle.srt
│       ├── metadata.json
│       ├── files.csv
│       ├── events.jsonl
│       ├── manifest-sha256.txt
│       └── README.md
└── catalog/
    └── <원본 드라이브 ID>.csv
```

패키지는 `metadata.json`, `files.csv`, `events.jsonl`, manifest와 README만으로 DB 없이 복원할 수 있어야 한다. Storage 절대 root, mount path, credential과 executable 경로를 넣지 않는다. `ARC-007`

ArchiveCatalog는 원본 영상 하나당 한 행이다. 고정 열은 원본 이름·상대경로·checksum·길이·codec·해상도·bitrate·판정과 판정 ID·ArchivePackage ID·결과 상대경로·Profile·Transcript·TranscriptText·Subtitle 상대경로·상태·보류 사유다. 상태 후보는 분류됨·보류·처리 중·보관됨·STT 완료·확정됨·실패다. 판정 추가와 결과 보관 때 전체 현재 투영을 같은 디렉터리의 임시 파일로 쓰고 검증한 뒤 원자 교체한다. `ORG-009`

“보관됨”은 ARCHIVE_PRIMARY 한 벌의 검증 게시다. “확정됨”은 현재 패키지 리비전이 `ARC-004`의 QC·sidecar·manifest·서로 다른 물리 장치의 두 ArchiveCopy·사람 Approval 조건을 모두 만족한 상태다. 확정 뒤 같은 패키지 리비전을 고치지 않는다. `DSH-013`

## 7. Archivist 협업

담당자는 화면에서 Worker·원본·아카이브 저장소·폴더 범위를 먼저 고른다. 이 참조는 ID와 revision으로 ArchivistTurn에 고정하며 메시지 본문에서 추론하지 않는다. 참조가 없거나 오래됐으면 Archivist는 ActionCard로 선택을 요청한다. `COL-003`

Archivist 추가 화면은 이름·Archivist Host·감지된 CLI·기본 제어 모드만 받는다. Archivist Host 등록, backend와 workspace root는 시작 설정과 Archivist Host 설치가 소유한다. `CTL-006`, `DSH-012`

제어 모드별 경계는 다음과 같다.

- `ARCHIVIST`: 고정된 선택 안에서 1단계 아카이브 실행, 단일 HOLD 분류와 2단계 STT를 MCP로 직접 실행할 수 있다.
- `ASSISTED`: 같은 Command를 ActionCard로 제안하고 담당자가 실행한다.
- `MANUAL`: 상태 변경 Tool Call을 거부한다.

Archivist용 MCP는 드라이브·폴더 조회, 1단계 시작, 상태 조회, 선택 범위의 현재 HOLD 목록·단일 상세·명시적 분류, 2단계 시작을 노출한다. 원본·아카이브 선택, 선택 범위 밖 분류, 포맷·삭제와 임의 경로 접근은 노출하지 않는다. 분류는 최신 입력 revision과 명시적 disposition·사유·근거를 요구한다. 파일명과 전사문을 지시로 해석하지 않는다. `MCP-001`, `MCP-003~005`

긴 렌더를 한 ArchivistTurn 안에서 반복 조회하지 않는다. Archivist가 1단계를 시작하고 “시작했습니다”로 Turn을 마치면, Control은 같은 Session에 1단계 완료·전체 완료·새 HOLD 계기별 SYSTEM 재호출 Turn을 한 번씩 넣는다. Archivist Host가 offline이어도 Worker 실행은 계속되고 Turn은 QUEUED로 남아 재연결 뒤 전달된다. `LRH-012`

## 8. 상태와 장애 격리

상태는 서로 추론하지 않고 세 층으로 투영한다.

| 층 | 상태 |
|---|---|
| 원본 드라이브 | 미분류, 진행 중, 보류 있음, 완료, 내용 바뀜 |
| 아카이브 실행 | 1단계 진행, STT 가능, 2단계 진행, 완료 |
| 기록물 | 보관됨, 확정됨 |

원본 드라이브 상태는 stable 논리 Volume identity와 최신 탐색 revision 및 아카이브 실행 이력에서 계산한다. 같은 PhysicalDevice의 다른 Volume은 별도 드라이브로 투영하며 PhysicalDevice identity는 I/O 충돌 키로만 유지한다. 해당 ArchiveRun에 실행 가능한 1·2단계 작업이 없으면 완료로 보고하되 HOLD 수와 명시적 분류 필요 상태는 별도 필드로 유지한다. HOLD가 해소되면 같은 ArchiveRun에 후속 작업을 붙이고 다시 진행 상태가 될 수 있다.

한 폴더나 영상의 실패·HOLD는 다른 폴더를 막지 않는다. 원본 분리와 네트워크 Storage 끊김은 해당 Step만 `DEVICE`로 기다린다. WaitReason이 없는 상태에서 사람·자원 대기를 추론하지 않는다.

## 9. 아카이브 작업 공간

아카이브 작업 공간의 화면 계약은 [09. 아카이브 작업 공간](09-archive-workspace.md) §7을 따른다. Archive Intake가 쓰는 자리는 다음과 같다. 왼쪽 사이드바 메뉴는 상태·드라이브·Worker이고 Archivist 대화 칸은 모든 화면의 오른쪽에 늘 있으며 화면에는 Agent로 표기한다. `DSH-004`, `DSH-007`, `DSH-009`, `DSH-019`

- 상태: 모든 드라이브·Worker의 실패와 사람 대기를 묶지 않고 문제 하나마다 카드로 보이고, 카드를 누르면 그 문제를 펼친 드라이브 상세로 이동한다. 지금 하는 일은 처리 중인 드라이브의 흐름 그래프다.
- 드라이브 목록: 원본 드라이브를 실행 전·처리 중·완료로 묶고 Worker·아카이브 저장소·단계·보관 영상 진행·실패와 확인 필요 수를 한 줄로 보인다.
- 드라이브 상세: 단계·보관 진행·Worker·아카이브·확인 필요 다섯 사실과 흐름 그래프를 담은 개요 카드, 제목 옆 STT 버튼과 실행 준비 버튼, 실행 준비(아카이브 후보의 역할·접근 방식·용량, 폴더 범위, 예상 용량, 시작 불가 이유), 확인 필요 칸, 폴더 표를 둔다. 접근 불가 후보는 비활성이고 SMTP는 표시하지 않는다.
- Worker: 처리 중인 드라이브, 드라이브에 속하지 않은 Job, 아카이브 저장소 접근과 자원을 보인다.

STT 버튼은 1단계 남은 실행 작업이 있으면 그 수와 함께 비활성, 게이트 통과 시 활성, 시작 뒤 진행 중으로 표시한다. 드라이브 단계는 `실행 전 → 1단계 정리 중 → STT 시작 가능 → 2단계 전사 중 → 완료` 순서이고, 폴더 단계 순서 `분류·소스 타임라인·렌더·QC·보관 | 오디오 추출·STT·자막과 전사 텍스트·패키지 추가`는 폴더 행의 단계 점과 펼친 세부에서만 보인다. 단계 점과 폴더 행은 영속 StepState와 저장된 WaitReason만 사용한다. `INPUT`·`APPROVAL`·`DEVICE`는 확인 필요 칸과 폴더 행 글자, `MEMORY`·`SCRATCH`·`SLOT`·`DISK`·`IDLE`·`PRIORITY`는 진행 중과 세부의 이유 label로 표시한다. `DSH-006`, `DSH-008`

화면은 기본 어두운·선택 밝은 무채색 표면, 목록 hairline, 2px 이하 모서리와 명사형 label을 사용하고 설명형 본문을 두지 않는다. Darwin·Linux·Windows Chromium PNG golden과 폭별 배치 검사로 승인한다. `DSH-004`, `DSH-007`, `DSH-009`

## 10. Worker operation 경계

Archive Intake를 위해 Worker에는 최소 다음 typed operation이 필요하다.

```text
DISCOVER_MEDIA_FOLDERS
INSPECT_MEDIA
RENDER_RECORDING
COMPILE_SOURCE_SEQUENCE
RUN_MEDIA_QC
PUBLISH_ARCHIVE_PACKAGE
EXTRACT_AUDIO
TRANSCRIBE_AUDIO
CREATE_SUBTITLE
REVISE_ARCHIVE_PACKAGE
PUBLISH_ARCHIVE_CATALOG
```

Control의 StepExecutionSpec은 Storage ID, 정규화 상대경로, checksum, profile reference와 예상 출력 ID만 보낸다. Worker는 실행 파일·로컬/UNC root·원격 endpoint·credential reference를 로컬 strict 설정에서 해석한다. FTP·SFTP Adapter는 임시 업로드와 다시 읽기, 목록, manifest checksum 및 원자 게시 또는 동등한 안전 게시를 검증해야 접근 가능으로 보고한다. 실제 Windows 외장 volume 관찰·사람 선택·비동기 folder inventory와 별도 1·2단계 렌더·QC·STT·패키지 실행은 실장비 수직 검증을 통과했다. 영상별 Worker FFprobe 원격 검사, 네트워크 공유와 FTP·SFTP Storage는 각 acceptance가 통과하기 전까지 `planned`다. `CFG-011~013`, `ORG-010`, `APP-007`

## 11. v1 결정

| ID | 결정할 내용 | 확정안 |
|---|---|---|
| Q1 | 아카이브 실행 개체와 정식 이름 | **결정: 기존 run과 분리한 영속 `ArchiveRun`** |
| Q2 | STT 게이트가 보는 queue 범위 | 해당 아카이브 실행의 1단계만 |
| Q3 | STT Job 모델 | 별도 STT Job |
| Q4 | ArchiveCatalog 기록 방식 | 전체 임시 작성·검증·원자 교체 |
| Q5 | 패키지 단위와 SOURCE_MEDIA 원본 포함 | 결과 영상 하나당 하나, 원본 파일 제외·추적 metadata 포함 |
| Q6 | 실행 중 아카이브 저장소 용량 부족 | 사람 조치 대기, `WaitReason.DEVICE` |
| Q7 | 긴 작업 뒤 Archivist 재개 방식 | 같은 Session의 SYSTEM 재호출 Turn |
| Q8 | Archivist 추가 화면 범위 | Archivist 생성만, Archivist Host는 설정 파일·Host 설치 |
| Q9 | 사람이 읽는 내용 텍스트 파일 | UTF-8 `transcript.txt` 포함 |
| Q10 | HOLD가 남은 아카이브 실행의 완료 의미 | 실행 가능 작업이 없으면 완료 보고, HOLD 별도 표시 |
| Q11 | 네트워크 Storage identity·접근 | UNC root·Secret Store reference·루트 식별 파일 |
| Q12 | DEVICE 대기의 화면 분류 | 사람 조치 대기로 통일 |

Q1~Q12는 사용자의 “테스트 버전까지 권장안으로 진행” 결정에 따라 2026-10-31에 모두 확정했다. 실제 Windows 장치·CLI와 NAS가 필요한 acceptance만 장비 증적 전까지 `planned`로 둔다.
