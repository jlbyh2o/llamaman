/**
 * `FormField`'s render props, narrowed for a control that does not take `undefined`.
 *
 * `FieldRenderProps` types its two ARIA members as `string | undefined` and `boolean | undefined`,
 * which is exactly right for a DOM element — React drops an undefined attribute. Radix-backed
 * controls declare them as plain optionals instead, and under `exactOptionalPropertyTypes` an
 * optional property is not the same thing as one that may be `undefined`, so spreading the render
 * props straight into `<Select>` is a type error rather than a shrug.
 *
 * This drops the absent members instead of passing them as `undefined`, which is the same thing at
 * runtime and the thing the compiler is asking for.
 */

import type { FieldRenderProps } from '../../components';

export interface NarrowedFieldProps {
  id: string;
  'aria-describedby'?: string;
  'aria-invalid'?: boolean;
}

export function fieldProps(field: FieldRenderProps): NarrowedFieldProps {
  const describedBy = field['aria-describedby'];
  const invalid = field['aria-invalid'];
  return {
    id: field.id,
    ...(describedBy === undefined ? {} : { 'aria-describedby': describedBy }),
    ...(invalid === undefined ? {} : { 'aria-invalid': invalid }),
  };
}
