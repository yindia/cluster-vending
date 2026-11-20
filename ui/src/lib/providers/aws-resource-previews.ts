import type { JsonObject } from "@bufbuild/protobuf";

export const awsPreviewServices = {
  s3: { label: "S3 bucket", description: "Object storage", example: { bucketNamespace: "global", forceDestroy: false } },
  rds: { label: "RDS database", description: "Relational database", example: { copyTagsToSnapshot: true, autoMinorVersionUpgrade: true, maintenanceWindow: "sun:03:00-sun:04:00" } },
  elasticache: { label: "ElastiCache cache", description: "In-memory cache", example: { applyImmediately: false, maintenanceWindow: "sun:04:00-sun:05:00" } },
} as const;

export type PreviewService = keyof typeof awsPreviewServices;
export type BucketControls = { blockPublicAccess: boolean; encryption: "AES256" | "aws:kms"; kmsKeyId: string };
export type AwsResourcePreview = {
  name: string;
  service: PreviewService;
  versioning?: boolean;
  bucketControls?: BucketControls;
  parameters?: JsonObject;
  // Older previews stored their summary and S3 versioning choice as display strings.
  settings?: string[];
};

export function isPreviewService(value: string): value is PreviewService {
  return Object.hasOwn(awsPreviewServices, value);
}

export function readPreviews(serialized: string): AwsResourcePreview[] {
  const rows: unknown = JSON.parse(serialized);
  if (!Array.isArray(rows) || !rows.every(row => row && typeof row.name === "string"
    && typeof row.service === "string" && isPreviewService(row.service)
    && (row.versioning === undefined || typeof row.versioning === "boolean")
    && (row.settings === undefined || (Array.isArray(row.settings) && row.settings.every((item: unknown) => typeof item === "string")))
    && (row.parameters === undefined ? !!row.settings : !!row.parameters && typeof row.parameters === "object" && !Array.isArray(row.parameters)))) {
    throw new Error("The saved previews in this tab could not be read.");
  }
  return rows;
}
