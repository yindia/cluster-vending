"use client";

import { FormLayout } from "@astryxdesign/core/FormLayout";
import { TextInput } from "@astryxdesign/core/TextInput";

export interface ResourceIdentity {
  name: string;
  displayName: string;
  region: string;
}

export interface ResourceIdentityFieldsProps {
  value: ResourceIdentity;
  onChange: (value: ResourceIdentity) => void;
  errors?: Partial<Record<keyof ResourceIdentity, string>>;
  isDisabled?: boolean;
  isNameReadOnly?: boolean;
}

/** Controlled identity fields; the owning form handles provider validation and submission. */
export function ResourceIdentityFields({
  value,
  onChange,
  errors = {},
  isDisabled = false,
  isNameReadOnly = false,
}: ResourceIdentityFieldsProps) {
  const status = (field: keyof ResourceIdentity) =>
    errors[field] ? { type: "error" as const, message: errors[field] } : undefined;

  return (
    <FormLayout direction="vertical">
      <TextInput
        label="Resource name"
        description="The stable identifier used by the API."
        value={value.name}
        onChange={(name) => onChange({ ...value, name })}
        isRequired
        isReadOnly={isNameReadOnly}
        isDisabled={isDisabled}
        autoComplete="off"
        autoCapitalize="none"
        hasAutoCorrect={false}
        hasSpellCheck={false}
        status={status("name")}
      />
      <TextInput
        label="Display name"
        description="An optional, human-readable name for this resource."
        value={value.displayName}
        onChange={(displayName) => onChange({ ...value, displayName })}
        isOptional
        isDisabled={isDisabled}
        autoComplete="off"
        status={status("displayName")}
      />
      <TextInput
        label="Region"
        description="The region identifier supported by the selected provider."
        value={value.region}
        onChange={(region) => onChange({ ...value, region })}
        isRequired
        isDisabled={isDisabled}
        autoComplete="off"
        autoCapitalize="none"
        hasAutoCorrect={false}
        hasSpellCheck={false}
        status={status("region")}
      />
    </FormLayout>
  );
}
