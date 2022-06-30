// Copyright 2022 Google LLC. All Rights Reserved.
// 
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
// 
//     http://www.apache.org/licenses/LICENSE-2.0
// 
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
// GENERATED BY gen_go_data.go
// gen_go_data -package beta -var YAML_ca_pool blaze-out/k8-fastbuild/genfiles/cloud/graphite/mmv2/services/google/privateca/beta/ca_pool.yaml

package beta

// blaze-out/k8-fastbuild/genfiles/cloud/graphite/mmv2/services/google/privateca/beta/ca_pool.yaml
var YAML_ca_pool = []byte("info:\n  title: Privateca/CaPool\n  description: The Privateca CaPool resource\n  x-dcl-struct-name: CaPool\n  x-dcl-has-iam: false\npaths:\n  get:\n    description: The function used to get information about a CaPool\n    parameters:\n    - name: CaPool\n      required: true\n      description: A full instance of a CaPool\n    timeoutSecs: 0\n  apply:\n    description: The function used to apply information about a CaPool\n    parameters:\n    - name: CaPool\n      required: true\n      description: A full instance of a CaPool\n    timeoutSecs: 0\n  delete:\n    description: The function used to delete a CaPool\n    parameters:\n    - name: CaPool\n      required: true\n      description: A full instance of a CaPool\n    timeoutSecs: 0\n  deleteAll:\n    description: The function used to delete all CaPool\n    parameters:\n    - name: project\n      required: true\n      schema:\n        type: string\n    - name: location\n      required: true\n      schema:\n        type: string\n    timeoutSecs: 0\n  list:\n    description: The function used to list information about many CaPool\n    parameters:\n    - name: project\n      required: true\n      schema:\n        type: string\n    - name: location\n      required: true\n      schema:\n        type: string\n    timeoutSecs: 0\ncomponents:\n  schemas:\n    CaPool:\n      title: CaPool\n      x-dcl-id: projects/{{project}}/locations/{{location}}/caPools/{{name}}\n      x-dcl-uses-state-hint: true\n      x-dcl-parent-container: project\n      x-dcl-labels: labels\n      x-dcl-has-create: true\n      x-dcl-has-iam: false\n      type: object\n      required:\n      - name\n      - tier\n      - project\n      - location\n      properties:\n        issuancePolicy:\n          type: object\n          x-dcl-go-name: IssuancePolicy\n          x-dcl-go-type: CaPoolIssuancePolicy\n          description: Optional. The IssuancePolicy to control how Certificates will\n            be issued from this CaPool.\n          properties:\n            allowedIssuanceModes:\n              type: object\n              x-dcl-go-name: AllowedIssuanceModes\n              x-dcl-go-type: CaPoolIssuancePolicyAllowedIssuanceModes\n              description: Optional. If specified, then only methods allowed in the\n                IssuanceModes may be used to issue Certificates.\n              properties:\n                allowConfigBasedIssuance:\n                  type: boolean\n                  x-dcl-go-name: AllowConfigBasedIssuance\n                  description: Optional. When true, allows callers to create Certificates\n                    by specifying a CertificateConfig.\n                allowCsrBasedIssuance:\n                  type: boolean\n                  x-dcl-go-name: AllowCsrBasedIssuance\n                  description: Optional. When true, allows callers to create Certificates\n                    by specifying a CSR.\n            allowedKeyTypes:\n              type: array\n              x-dcl-go-name: AllowedKeyTypes\n              description: Optional. If any AllowedKeyType is specified, then the\n                certificate request's public key must match one of the key types listed\n                here. Otherwise, any key may be used.\n              x-dcl-send-empty: true\n              x-dcl-list-type: list\n              items:\n                type: object\n                x-dcl-go-type: CaPoolIssuancePolicyAllowedKeyTypes\n                properties:\n                  ellipticCurve:\n                    type: object\n                    x-dcl-go-name: EllipticCurve\n                    x-dcl-go-type: CaPoolIssuancePolicyAllowedKeyTypesEllipticCurve\n                    description: Represents an allowed Elliptic Curve key type.\n                    x-dcl-conflicts:\n                    - rsa\n                    properties:\n                      signatureAlgorithm:\n                        type: string\n                        x-dcl-go-name: SignatureAlgorithm\n                        x-dcl-go-type: CaPoolIssuancePolicyAllowedKeyTypesEllipticCurveSignatureAlgorithmEnum\n                        description: 'Optional. A signature algorithm that must be\n                          used. If this is omitted, any EC-based signature algorithm\n                          will be allowed. Possible values: EC_SIGNATURE_ALGORITHM_UNSPECIFIED,\n                          ECDSA_P256, ECDSA_P384, EDDSA_25519'\n                        enum:\n                        - EC_SIGNATURE_ALGORITHM_UNSPECIFIED\n                        - ECDSA_P256\n                        - ECDSA_P384\n                        - EDDSA_25519\n                  rsa:\n                    type: object\n                    x-dcl-go-name: Rsa\n                    x-dcl-go-type: CaPoolIssuancePolicyAllowedKeyTypesRsa\n                    description: Represents an allowed RSA key type.\n                    x-dcl-conflicts:\n                    - ellipticCurve\n                    properties:\n                      maxModulusSize:\n                        type: integer\n                        format: int64\n                        x-dcl-go-name: MaxModulusSize\n                        description: Optional. The maximum allowed RSA modulus size,\n                          in bits. If this is not set, or if set to zero, the service\n                          will not enforce an explicit upper bound on RSA modulus\n                          sizes.\n                      minModulusSize:\n                        type: integer\n                        format: int64\n                        x-dcl-go-name: MinModulusSize\n                        description: Optional. The minimum allowed RSA modulus size,\n                          in bits. If this is not set, or if set to zero, the service-level\n                          min RSA modulus size will continue to apply.\n            baselineValues:\n              type: object\n              x-dcl-go-name: BaselineValues\n              x-dcl-go-type: CaPoolIssuancePolicyBaselineValues\n              description: Optional. A set of X.509 values that will be applied to\n                all certificates issued through this CaPool. If a certificate request\n                includes conflicting values for the same properties, they will be\n                overwritten by the values defined here. If a certificate request uses\n                a CertificateTemplate that defines conflicting predefined_values for\n                the same properties, the certificate issuance request will fail.\n              properties:\n                additionalExtensions:\n                  type: array\n                  x-dcl-go-name: AdditionalExtensions\n                  description: Optional. Describes custom X.509 extensions.\n                  x-dcl-send-empty: true\n                  x-dcl-list-type: list\n                  items:\n                    type: object\n                    x-dcl-go-type: CaPoolIssuancePolicyBaselineValuesAdditionalExtensions\n                    required:\n                    - objectId\n                    - value\n                    properties:\n                      critical:\n                        type: boolean\n                        x-dcl-go-name: Critical\n                        description: Optional. Indicates whether or not this extension\n                          is critical (i.e., if the client does not know how to handle\n                          this extension, the client should consider this to be an\n                          error).\n                      objectId:\n                        type: object\n                        x-dcl-go-name: ObjectId\n                        x-dcl-go-type: CaPoolIssuancePolicyBaselineValuesAdditionalExtensionsObjectId\n                        description: Required. The OID for this X.509 extension.\n                        required:\n                        - objectIdPath\n                        properties:\n                          objectIdPath:\n                            type: array\n                            x-dcl-go-name: ObjectIdPath\n                            description: Required. The parts of an OID path. The most\n                              significant parts of the path come first.\n                            x-dcl-send-empty: true\n                            x-dcl-list-type: list\n                            items:\n                              type: integer\n                              format: int64\n                              x-dcl-go-type: int64\n                      value:\n                        type: string\n                        x-dcl-go-name: Value\n                        description: Required. The value of this X.509 extension.\n                aiaOcspServers:\n                  type: array\n                  x-dcl-go-name: AiaOcspServers\n                  description: Optional. Describes Online Certificate Status Protocol\n                    (OCSP) endpoint addresses that appear in the \"Authority Information\n                    Access\" extension in the certificate.\n                  x-dcl-send-empty: true\n                  x-dcl-list-type: list\n                  items:\n                    type: string\n                    x-dcl-go-type: string\n                caOptions:\n                  type: object\n                  x-dcl-go-name: CaOptions\n                  x-dcl-go-type: CaPoolIssuancePolicyBaselineValuesCaOptions\n                  description: Optional. Describes options in this X509Parameters\n                    that are relevant in a CA certificate.\n                  properties:\n                    isCa:\n                      type: boolean\n                      x-dcl-go-name: IsCa\n                      description: Optional. Refers to the \"CA\" X.509 extension, which\n                        is a boolean value. When this value is missing, the extension\n                        will be omitted from the CA certificate.\n                    maxIssuerPathLength:\n                      type: integer\n                      format: int64\n                      x-dcl-go-name: MaxIssuerPathLength\n                      description: Optional. Refers to the path length restriction\n                        X.509 extension. For a CA certificate, this value describes\n                        the depth of subordinate CA certificates that are allowed.\n                        If this value is less than 0, the request will fail. If this\n                        value is missing, the max path length will be omitted from\n                        the CA certificate.\n                      x-dcl-mutable-unreadable: true\n                keyUsage:\n                  type: object\n                  x-dcl-go-name: KeyUsage\n                  x-dcl-go-type: CaPoolIssuancePolicyBaselineValuesKeyUsage\n                  description: Optional. Indicates the intended use for keys that\n                    correspond to a certificate.\n                  properties:\n                    baseKeyUsage:\n                      type: object\n                      x-dcl-go-name: BaseKeyUsage\n                      x-dcl-go-type: CaPoolIssuancePolicyBaselineValuesKeyUsageBaseKeyUsage\n                      description: Describes high-level ways in which a key may be\n                        used.\n                      properties:\n                        certSign:\n                          type: boolean\n                          x-dcl-go-name: CertSign\n                          description: The key may be used to sign certificates.\n                        contentCommitment:\n                          type: boolean\n                          x-dcl-go-name: ContentCommitment\n                          description: The key may be used for cryptographic commitments.\n                            Note that this may also be referred to as \"non-repudiation\".\n                        crlSign:\n                          type: boolean\n                          x-dcl-go-name: CrlSign\n                          description: The key may be used sign certificate revocation\n                            lists.\n                        dataEncipherment:\n                          type: boolean\n                          x-dcl-go-name: DataEncipherment\n                          description: The key may be used to encipher data.\n                        decipherOnly:\n                          type: boolean\n                          x-dcl-go-name: DecipherOnly\n                          description: The key may be used to decipher only.\n                        digitalSignature:\n                          type: boolean\n                          x-dcl-go-name: DigitalSignature\n                          description: The key may be used for digital signatures.\n                        encipherOnly:\n                          type: boolean\n                          x-dcl-go-name: EncipherOnly\n                          description: The key may be used to encipher only.\n                        keyAgreement:\n                          type: boolean\n                          x-dcl-go-name: KeyAgreement\n                          description: The key may be used in a key agreement protocol.\n                        keyEncipherment:\n                          type: boolean\n                          x-dcl-go-name: KeyEncipherment\n                          description: The key may be used to encipher other keys.\n                    extendedKeyUsage:\n                      type: object\n                      x-dcl-go-name: ExtendedKeyUsage\n                      x-dcl-go-type: CaPoolIssuancePolicyBaselineValuesKeyUsageExtendedKeyUsage\n                      description: Detailed scenarios in which a key may be used.\n                      properties:\n                        clientAuth:\n                          type: boolean\n                          x-dcl-go-name: ClientAuth\n                          description: Corresponds to OID 1.3.6.1.5.5.7.3.2. Officially\n                            described as \"TLS WWW client authentication\", though regularly\n                            used for non-WWW TLS.\n                        codeSigning:\n                          type: boolean\n                          x-dcl-go-name: CodeSigning\n                          description: Corresponds to OID 1.3.6.1.5.5.7.3.3. Officially\n                            described as \"Signing of downloadable executable code\n                            client authentication\".\n                        emailProtection:\n                          type: boolean\n                          x-dcl-go-name: EmailProtection\n                          description: Corresponds to OID 1.3.6.1.5.5.7.3.4. Officially\n                            described as \"Email protection\".\n                        ocspSigning:\n                          type: boolean\n                          x-dcl-go-name: OcspSigning\n                          description: Corresponds to OID 1.3.6.1.5.5.7.3.9. Officially\n                            described as \"Signing OCSP responses\".\n                        serverAuth:\n                          type: boolean\n                          x-dcl-go-name: ServerAuth\n                          description: Corresponds to OID 1.3.6.1.5.5.7.3.1. Officially\n                            described as \"TLS WWW server authentication\", though regularly\n                            used for non-WWW TLS.\n                        timeStamping:\n                          type: boolean\n                          x-dcl-go-name: TimeStamping\n                          description: Corresponds to OID 1.3.6.1.5.5.7.3.8. Officially\n                            described as \"Binding the hash of an object to a time\".\n                    unknownExtendedKeyUsages:\n                      type: array\n                      x-dcl-go-name: UnknownExtendedKeyUsages\n                      description: Used to describe extended key usages that are not\n                        listed in the KeyUsage.ExtendedKeyUsageOptions message.\n                      x-dcl-send-empty: true\n                      x-dcl-list-type: list\n                      items:\n                        type: object\n                        x-dcl-go-type: CaPoolIssuancePolicyBaselineValuesKeyUsageUnknownExtendedKeyUsages\n                        required:\n                        - objectIdPath\n                        properties:\n                          objectIdPath:\n                            type: array\n                            x-dcl-go-name: ObjectIdPath\n                            description: Required. The parts of an OID path. The most\n                              significant parts of the path come first.\n                            x-dcl-send-empty: true\n                            x-dcl-list-type: list\n                            items:\n                              type: integer\n                              format: int64\n                              x-dcl-go-type: int64\n                policyIds:\n                  type: array\n                  x-dcl-go-name: PolicyIds\n                  description: Optional. Describes the X.509 certificate policy object\n                    identifiers, per https://tools.ietf.org/html/rfc5280#section-4.2.1.4.\n                  x-dcl-send-empty: true\n                  x-dcl-list-type: list\n                  items:\n                    type: object\n                    x-dcl-go-type: CaPoolIssuancePolicyBaselineValuesPolicyIds\n                    required:\n                    - objectIdPath\n                    properties:\n                      objectIdPath:\n                        type: array\n                        x-dcl-go-name: ObjectIdPath\n                        description: Required. The parts of an OID path. The most\n                          significant parts of the path come first.\n                        x-dcl-send-empty: true\n                        x-dcl-list-type: list\n                        items:\n                          type: integer\n                          format: int64\n                          x-dcl-go-type: int64\n            identityConstraints:\n              type: object\n              x-dcl-go-name: IdentityConstraints\n              x-dcl-go-type: CaPoolIssuancePolicyIdentityConstraints\n              description: Optional. Describes constraints on identities that may\n                appear in Certificates issued through this CaPool. If this is omitted,\n                then this CaPool will not add restrictions on a certificate's identity.\n              required:\n              - allowSubjectPassthrough\n              - allowSubjectAltNamesPassthrough\n              properties:\n                allowSubjectAltNamesPassthrough:\n                  type: boolean\n                  x-dcl-go-name: AllowSubjectAltNamesPassthrough\n                  description: Required. If this is true, the SubjectAltNames extension\n                    may be copied from a certificate request into the signed certificate.\n                    Otherwise, the requested SubjectAltNames will be discarded.\n                allowSubjectPassthrough:\n                  type: boolean\n                  x-dcl-go-name: AllowSubjectPassthrough\n                  description: Required. If this is true, the Subject field may be\n                    copied from a certificate request into the signed certificate.\n                    Otherwise, the requested Subject will be discarded.\n                celExpression:\n                  type: object\n                  x-dcl-go-name: CelExpression\n                  x-dcl-go-type: CaPoolIssuancePolicyIdentityConstraintsCelExpression\n                  description: Optional. A CEL expression that may be used to validate\n                    the resolved X.509 Subject and/or Subject Alternative Name before\n                    a certificate is signed. To see the full allowed syntax and some\n                    examples, see https://cloud.google.com/certificate-authority-service/docs/using-cel\n                  properties:\n                    description:\n                      type: string\n                      x-dcl-go-name: Description\n                      description: Optional. Description of the expression. This is\n                        a longer text which describes the expression, e.g. when hovered\n                        over it in a UI.\n                    expression:\n                      type: string\n                      x-dcl-go-name: Expression\n                      description: Textual representation of an expression in Common\n                        Expression Language syntax.\n                    location:\n                      type: string\n                      x-dcl-go-name: Location\n                      description: Optional. String indicating the location of the\n                        expression for error reporting, e.g. a file name and a position\n                        in the file.\n                    title:\n                      type: string\n                      x-dcl-go-name: Title\n                      description: Optional. Title for the expression, i.e. a short\n                        string describing its purpose. This can be used e.g. in UIs\n                        which allow to enter the expression.\n            maximumLifetime:\n              type: string\n              x-dcl-go-name: MaximumLifetime\n              description: Optional. The maximum lifetime allowed for issued Certificates.\n                Note that if the issuing CertificateAuthority expires before a Certificate's\n                requested maximum_lifetime, the effective lifetime will be explicitly\n                truncated to match it.\n            passthroughExtensions:\n              type: object\n              x-dcl-go-name: PassthroughExtensions\n              x-dcl-go-type: CaPoolIssuancePolicyPassthroughExtensions\n              description: Optional. Describes the set of X.509 extensions that may\n                appear in a Certificate issued through this CaPool. If a certificate\n                request sets extensions that don't appear in the passthrough_extensions,\n                those extensions will be dropped. If a certificate request uses a\n                CertificateTemplate with predefined_values that don't appear here,\n                the certificate issuance request will fail. If this is omitted, then\n                this CaPool will not add restrictions on a certificate's X.509 extensions.\n                These constraints do not apply to X.509 extensions set in this CaPool's\n                baseline_values.\n              properties:\n                additionalExtensions:\n                  type: array\n                  x-dcl-go-name: AdditionalExtensions\n                  description: Optional. A set of ObjectIds identifying custom X.509\n                    extensions. Will be combined with known_extensions to determine\n                    the full set of X.509 extensions.\n                  x-dcl-send-empty: true\n                  x-dcl-list-type: list\n                  items:\n                    type: object\n                    x-dcl-go-type: CaPoolIssuancePolicyPassthroughExtensionsAdditionalExtensions\n                    required:\n                    - objectIdPath\n                    properties:\n                      objectIdPath:\n                        type: array\n                        x-dcl-go-name: ObjectIdPath\n                        description: Required. The parts of an OID path. The most\n                          significant parts of the path come first.\n                        x-dcl-send-empty: true\n                        x-dcl-list-type: list\n                        items:\n                          type: integer\n                          format: int64\n                          x-dcl-go-type: int64\n                knownExtensions:\n                  type: array\n                  x-dcl-go-name: KnownExtensions\n                  description: Optional. A set of named X.509 extensions. Will be\n                    combined with additional_extensions to determine the full set\n                    of X.509 extensions.\n                  x-dcl-send-empty: true\n                  x-dcl-list-type: list\n                  items:\n                    type: string\n                    x-dcl-go-type: CaPoolIssuancePolicyPassthroughExtensionsKnownExtensionsEnum\n                    enum:\n                    - KNOWN_CERTIFICATE_EXTENSION_UNSPECIFIED\n                    - BASE_KEY_USAGE\n                    - EXTENDED_KEY_USAGE\n                    - CA_OPTIONS\n                    - POLICY_IDS\n                    - AIA_OCSP_SERVERS\n        labels:\n          type: object\n          additionalProperties:\n            type: string\n          x-dcl-go-name: Labels\n          description: Optional. Labels with user-defined metadata.\n        location:\n          type: string\n          x-dcl-go-name: Location\n          description: The location for the resource\n          x-kubernetes-immutable: true\n        name:\n          type: string\n          x-dcl-go-name: Name\n          description: The resource name for this CaPool in the format `projects/*/locations/*/caPools/*`.\n          x-kubernetes-immutable: true\n        project:\n          type: string\n          x-dcl-go-name: Project\n          description: The project for the resource\n          x-kubernetes-immutable: true\n          x-dcl-references:\n          - resource: Cloudresourcemanager/Project\n            field: name\n            parent: true\n        publishingOptions:\n          type: object\n          x-dcl-go-name: PublishingOptions\n          x-dcl-go-type: CaPoolPublishingOptions\n          description: Optional. The PublishingOptions to follow when issuing Certificates\n            from any CertificateAuthority in this CaPool.\n          properties:\n            publishCaCert:\n              type: boolean\n              x-dcl-go-name: PublishCaCert\n              description: Optional. When true, publishes each CertificateAuthority's\n                CA certificate and includes its URL in the \"Authority Information\n                Access\" X.509 extension in all issued Certificates. If this is false,\n                the CA certificate will not be published and the corresponding X.509\n                extension will not be written in issued certificates.\n            publishCrl:\n              type: boolean\n              x-dcl-go-name: PublishCrl\n              description: Optional. When true, publishes each CertificateAuthority's\n                CRL and includes its URL in the \"CRL Distribution Points\" X.509 extension\n                in all issued Certificates. If this is false, CRLs will not be published\n                and the corresponding X.509 extension will not be written in issued\n                certificates. CRLs will expire 7 days from their creation. However,\n                we will rebuild daily. CRLs are also rebuilt shortly after a certificate\n                is revoked.\n        tier:\n          type: string\n          x-dcl-go-name: Tier\n          x-dcl-go-type: CaPoolTierEnum\n          description: 'Required. Immutable. The Tier of this CaPool. Possible values:\n            TIER_UNSPECIFIED, ENTERPRISE, DEVOPS'\n          x-kubernetes-immutable: true\n          enum:\n          - TIER_UNSPECIFIED\n          - ENTERPRISE\n          - DEVOPS\n")

// 27081 bytes
// MD5: 845de6f03ab8cbcfd8b74c50cb6e4ff9