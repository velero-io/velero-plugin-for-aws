## All changes

- Add support for specifying KMS key for EBS volume restoration (#276, @tropnikovvl)
- Fix SSE-C signed URLs (#280, @lukasgabriel)
- Add license scan report and status (#300, @fossabot)
- Only set PutObject Tagging when tags are configured (#299, @dillonbrowne)
- Bump aws-sdk-go-v2 for AWS European Sovereign Cloud support (#293, @ahsanemon)
- Fix prevent amz-checksum encapsulation when checksumAlgorithm="" (#308, @clementnuss)
- fix: allow valid S3 object tag values (#294, @immanuwell)
- Remove community health files now provided by org-level .github repo (#314, @shubham-pampattiwar)
- Add descriptive error message when SSE-C is denied on S3 buckets (#318, @shubham-pampattiwar)
- Replace panics with contextual error messages in cp-plugin (#331, @chlins)
- Update the module from org "vmware-tanzu" to "velero-io" (#337, @reasonerjt)
- Make the S3 uploader inherit the client's RequestChecksumCalculation (#335, @dee-kryvenko)
